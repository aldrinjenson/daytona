// Copyright Daytona Platforms Inc.
// SPDX-License-Identifier: Apache-2.0

package io.daytona.sdk;

import java.util.function.Consumer;

public class DownloadStreamOptions {
    private int timeoutSeconds = 30 * 60;
    private Consumer<Long> onProgress;

    public int getTimeoutSeconds() { return timeoutSeconds; }
    public Consumer<Long> getOnProgress() { return onProgress; }

    public DownloadStreamOptions setTimeout(int timeoutSeconds) {
        this.timeoutSeconds = timeoutSeconds;
        return this;
    }

    public DownloadStreamOptions setOnProgress(Consumer<Long> onProgress) {
        this.onProgress = onProgress;
        return this;
    }
}
