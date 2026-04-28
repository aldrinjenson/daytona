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
// MountAll never errors fatally: a failed mount is logged and skipped so the
// rest of the daemon can still come up. Callers should surface readiness
// signals through their own paths.
//
// As a defensive measure the env vars carrying the volume spec (which contain
// per-disk Archil mount tokens) are scrubbed from the daemon's own process
// environment before returning. Child processes spawned later by the daemon
// or by user code will not inherit them.
func MountAll(ctx context.Context, logger *slog.Logger) {
	defer scrubEnv(logger)

	raw := os.Getenv(envVolumesJSON)
	if raw == "" {
		return
	}

	binary := os.Getenv(envArchilBinary)
	if binary == "" {
		logger.Warn("in-container volume spec present but archil binary path is empty", "env", envArchilBinary)
		return
	}
	if _, err := os.Stat(binary); err != nil {
		logger.Warn("in-container archil binary not found; skipping volume mounts", "path", binary, "error", err)
		return
	}

	var volumes []Volume
	if err := json.Unmarshal([]byte(raw), &volumes); err != nil {
		logger.Error("failed to parse in-container volume spec", "error", err)
		return
	}

	for _, v := range volumes {
		if err := mountOne(ctx, logger, binary, v); err != nil {
			logger.Error(
				"failed to mount in-container volume",
				"volumeId", v.VolumeID,
				"mountPath", v.MountPath,
				"archilDisk", v.ArchilDisk,
				"archilRegion", v.ArchilRegion,
				"error", err,
			)
			continue
		}
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
