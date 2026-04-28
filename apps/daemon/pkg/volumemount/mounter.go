// Copyright Daytona Platforms Inc.
// SPDX-License-Identifier: AGPL-3.0

// Package volumemount runs inside the sandbox container and performs the
// in-container volume mount driven by the env payload injected by the runner.
// This is the sandbox-side counterpart of pkg/volume/incontainer in the
// runner.
//
// Backend: Archil. Each volume is mounted with `archil mount <DISK>
// <MOUNTPOINT> --region <REGION>`, authenticated by a per-disk
// ARCHIL_MOUNT_TOKEN passed via the child process environment (never on the
// command line, so it can't leak via /proc/<pid>/cmdline or `ps`).
//
// Env contract (must match pkg/volume/incontainer in runner):
//
//	DAYTONA_INCONTAINER_VOLUMES         JSON-encoded []Volume (with per-volume
//	                                    archilDisk / archilRegion / archilMountToken)
//	DAYTONA_INCONTAINER_ARCHIL_BINARY   absolute path to the archil CLI binary
package volumemount

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const (
	envVolumesJSON  = "DAYTONA_INCONTAINER_VOLUMES"
	envArchilBinary = "DAYTONA_INCONTAINER_ARCHIL_BINARY"
)

// Volume mirrors volume.Volume in the runner. Only the fields the daemon
// actually consumes are declared here.
type Volume struct {
	VolumeID         string `json:"volumeId"`
	MountPath        string `json:"mountPath"`
	Subpath          string `json:"subpath,omitempty"`
	ReadOnly         bool   `json:"readOnly,omitempty"`
	ArchilDisk       string `json:"archilDisk,omitempty"`
	ArchilRegion     string `json:"archilRegion,omitempty"`
	ArchilMountToken string `json:"archilMountToken,omitempty"`
}

// MountAll reads the env payload and mounts every declared volume. It is
// idempotent — already-mounted paths are skipped.
//
// Each volume is attempted up to mountMaxAttempts times before giving up.
// If any volume fails after all retries, MountAll returns an error so the
// daemon can exit non-zero and the runner can surface the failure as a
// sandbox-level error rather than letting the sandbox come up with empty
// mount paths.
//
// As a defensive measure the env vars carrying the volume spec (which contain
// per-disk Archil mount tokens) are scrubbed from the daemon's own process
// environment before returning. Child processes spawned later by the daemon
// or by user code will not inherit them.
func MountAll(ctx context.Context, logger *slog.Logger) error {
	defer scrubEnv(logger)

	raw := os.Getenv(envVolumesJSON)
	if raw == "" {
		return nil
	}

	binary := os.Getenv(envArchilBinary)
	if binary == "" {
		return fmt.Errorf("in-container volume spec present but %s is empty", envArchilBinary)
	}
	if _, err := os.Stat(binary); err != nil {
		return fmt.Errorf("in-container archil binary not found at %q: %w", binary, err)
	}

	var volumes []Volume
	if err := json.Unmarshal([]byte(raw), &volumes); err != nil {
		return fmt.Errorf("parse in-container volume spec: %w", err)
	}

	var failures []error
	for _, v := range volumes {
		if err := mountOneWithRetry(ctx, logger, binary, v); err != nil {
			logger.Error(
				"failed to mount in-container volume after retries",
				"volumeId", v.VolumeID,
				"mountPath", v.MountPath,
				"archilDisk", v.ArchilDisk,
				"archilRegion", v.ArchilRegion,
				"attempts", mountMaxAttempts,
				"error", err,
			)
			failures = append(failures, fmt.Errorf("volume %q at %q: %w", v.VolumeID, v.MountPath, err))
		}
	}

	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	return nil
}

const (
	// mountMaxAttempts is the total number of attempts per volume (one
	// initial attempt + retries). Most failures are deterministic
	// (bad token, deleted disk, wrong region) so retries don't help, but a
	// short retry window absorbs transient network glitches without making
	// users wait long when the failure is permanent.
	mountMaxAttempts = 3
	// mountRetryBackoff is the fixed sleep between retry attempts. We don't
	// bother with exponential backoff because the per-attempt 5s readiness
	// timeout already paces us, and the overall mount budget (30s in
	// daemon main) caps the total wait.
	mountRetryBackoff = 1 * time.Second
)

// mountOneWithRetry calls mountOne up to mountMaxAttempts times. Between
// attempts it best-effort-cleans up any half-mounted state so the next
// attempt isn't fooled by a stale mountpoint.
func mountOneWithRetry(ctx context.Context, logger *slog.Logger, binary string, v Volume) error {
	var lastErr error
	for attempt := 1; attempt <= mountMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("aborting volume mount on attempt %d/%d: %w", attempt, mountMaxAttempts, err)
		}

		err := mountOne(ctx, logger, binary, v)
		if err == nil {
			if attempt > 1 {
				logger.Info(
					"in-container volume mounted on retry",
					"volumeId", v.VolumeID,
					"mountPath", v.MountPath,
					"attempt", attempt,
				)
			}
			return nil
		}
		lastErr = err

		if attempt == mountMaxAttempts {
			break
		}

		logger.Warn(
			"in-container volume mount failed; will retry",
			"volumeId", v.VolumeID,
			"mountPath", v.MountPath,
			"attempt", attempt,
			"maxAttempts", mountMaxAttempts,
			"error", err,
		)

		// A failed `archil mount` may have left the FUSE mountpoint
		// half-registered. Attempt a best-effort unmount before retrying
		// so mountOne's "already mounted" shortcut doesn't return a
		// stale success on the next pass.
		bestEffortUnmount(ctx, logger, binary, v.MountPath)

		select {
		case <-ctx.Done():
			return fmt.Errorf("aborting volume mount during retry backoff: %w (last error: %v)", ctx.Err(), lastErr)
		case <-time.After(mountRetryBackoff):
		}
	}
	return lastErr
}

// bestEffortUnmount tries `archil unmount` first (which flushes pending
// writes and tears down the FUSE server cleanly), and falls back to
// `umount -l` if archil didn't manage. Any failure is logged at Warn and
// otherwise ignored — the caller is about to retry the mount, and starting
// from a clean state is preferred but not required.
func bestEffortUnmount(ctx context.Context, logger *slog.Logger, binary string, mountPath string) {
	if !isMountpoint(mountPath) {
		return
	}
	logger.Debug("unmounting half-mounted path before retry", "mountPath", mountPath)

	if err := exec.CommandContext(ctx, binary, "unmount", mountPath).Run(); err == nil {
		return
	}
	if err := exec.CommandContext(ctx, "umount", "-l", mountPath).Run(); err != nil {
		logger.Warn("best-effort unmount failed before retry", "mountPath", mountPath, "error", err)
	}
}

func mountOne(ctx context.Context, logger *slog.Logger, binary string, v Volume) error {
	if v.MountPath == "" {
		return fmt.Errorf("invalid volume entry: empty mountPath")
	}
	if v.ArchilDisk == "" {
		return fmt.Errorf("invalid volume entry: empty archilDisk")
	}
	if v.ArchilRegion == "" {
		return fmt.Errorf("invalid volume entry: empty archilRegion")
	}
	if v.ArchilMountToken == "" {
		return fmt.Errorf("invalid volume entry: empty archilMountToken")
	}

	if err := os.MkdirAll(v.MountPath, 0755); err != nil {
		return fmt.Errorf("create mountpoint: %w", err)
	}

	if isMountpoint(v.MountPath) {
		logger.Debug("volume already mounted in-container", "volumeId", v.VolumeID, "mountPath", v.MountPath)
		return nil
	}

	// archil supports `disk[:/subpath]` syntax to mount a subdirectory of
	// the disk as the mount root, mirroring NFS conventions.
	target := v.ArchilDisk
	if v.Subpath != "" {
		sub := v.Subpath
		if sub[0] != '/' {
			sub = "/" + sub
		}
		target = v.ArchilDisk + ":" + sub
	}

	args := []string{
		"mount",
		target,
		v.MountPath,
		"--region", v.ArchilRegion,
	}
	if v.ReadOnly {
		// `--read-only` was added in archil client v0.5.0. Read-only
		// mounts don't take a write delegation, so multiple sandboxes
		// can hold concurrent RO views of the same disk while a separate
		// RW mount is active elsewhere.
		args = append(args, "--read-only")
	}

	cmd := exec.CommandContext(ctx, binary, args...)
	// Pass the token via env, not argv: argv is visible in /proc/<pid>/cmdline
	// and `ps`, env (for processes the daemon doesn't own) is not. The archil
	// CLI itself reads ARCHIL_MOUNT_TOKEN from env.
	cmd.Env = append(os.Environ(), "ARCHIL_MOUNT_TOKEN="+v.ArchilMountToken)

	logger.Info(
		"mounting in-container volume",
		"volumeId", v.VolumeID,
		"mountPath", v.MountPath,
		"archilDisk", v.ArchilDisk,
		"archilRegion", v.ArchilRegion,
		"subpath", v.Subpath,
		"readOnly", v.ReadOnly,
	)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("archil mount failed: %w: %s", err, string(out))
	}

	if err := waitUntilReady(ctx, v.MountPath); err != nil {
		return fmt.Errorf("mount not ready: %w", err)
	}
	logger.Info("mounted in-container volume", "volumeId", v.VolumeID, "mountPath", v.MountPath)
	return nil
}

// scrubEnv unsets the env vars that carry the volume spec (which contain
// per-disk mount tokens) from the daemon's own process environment, so they
// don't leak into child processes spawned later or get printed by anything
// that dumps `os.Environ()`.
//
// The archil mounts themselves are unaffected — once `archil mount` returns,
// the FUSE server it forked off no longer needs ARCHIL_MOUNT_TOKEN.
func scrubEnv(logger *slog.Logger) {
	for _, k := range []string{envVolumesJSON, envArchilBinary} {
		if err := os.Unsetenv(k); err != nil {
			logger.Warn("failed to unset in-container env var", "var", k, "error", err)
		}
	}
}

func isMountpoint(path string) bool {
	cleaned := filepath.Clean(path)
	parent := filepath.Dir(cleaned)

	pi, err := os.Stat(cleaned)
	if err != nil {
		return false
	}
	pp, err := os.Stat(parent)
	if err != nil {
		return false
	}

	pDev, ok1 := statDev(pi)
	parentDev, ok2 := statDev(pp)
	if !ok1 || !ok2 {
		return false
	}
	return pDev != parentDev
}

func waitUntilReady(ctx context.Context, path string) error {
	const maxAttempts = 50
	const sleep = 100 * time.Millisecond

	for i := 0; i < maxAttempts; i++ {
		if !isMountpoint(path) {
			return fmt.Errorf("mount disappeared during readiness check")
		}
		if _, err := os.ReadDir(path); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(sleep):
		}
	}
	return fmt.Errorf("mount did not become ready within timeout")
}
