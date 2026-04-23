// Copyright 2025 Daytona Platforms Inc.
// SPDX-License-Identifier: AGPL-3.0

package docker

import (
	"context"
	"fmt"

	"github.com/daytonaio/runner/pkg/models/enums"
)

func (d *DockerClient) Pause(ctx context.Context, containerId string) error {
	state, err := d.GetSandboxState(ctx, containerId)
	if err == nil && state == enums.SandboxStatePaused {
		d.logger.DebugContext(ctx, "Sandbox is already paused", "containerId", containerId)
		return nil
	}

	if err != nil {
		d.logger.WarnContext(ctx, "Failed to get sandbox state", "containerId", containerId, "error", err)
		d.logger.WarnContext(ctx, "Continuing with pause operation")
	}

	err = d.apiClient.ContainerPause(ctx, containerId)
	if err != nil {
		return fmt.Errorf("error pausing sandbox %s: %w", containerId, err)
	}

	d.logger.DebugContext(ctx, "Sandbox paused successfully", "containerId", containerId)
	return nil
}
