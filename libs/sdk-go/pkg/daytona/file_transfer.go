// Copyright Daytona Platforms Inc.
// SPDX-License-Identifier: Apache-2.0

package daytona

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/daytonaio/daytona/libs/sdk-go/pkg/errors"
	"github.com/daytonaio/daytona/libs/toolbox-api-client-go"
)

type downloadStreamCloser struct {
	partReader io.Reader
	response   *http.Response
}

type progressReader struct {
	inner      io.Reader
	onProgress func(int64)
	total      int64
}

func (d *downloadStreamCloser) Read(p []byte) (int, error) {
	return d.partReader.Read(p)
}

func (p *progressReader) Read(buf []byte) (int, error) {
	n, err := p.inner.Read(buf)
	if n > 0 {
		p.total += int64(n)
		p.onProgress(p.total)
	}
	return n, err
}

func (d *downloadStreamCloser) Close() error {
	if d.response == nil || d.response.Body == nil {
		return nil
	}
	return d.response.Body.Close()
}

func streamDownloadFile(cfg *toolbox.Configuration, remotePath string, ctx context.Context, onProgress func(int64)) (io.ReadCloser, error) {
	if len(cfg.Servers) == 0 {
		return nil, errors.NewDaytonaError("Toolbox client is not configured", 0, nil)
	}

	requestBody, err := json.Marshal(toolbox.NewFilesDownloadRequest([]string{remotePath}))
	if err != nil {
		return nil, errors.NewDaytonaError(fmt.Sprintf("Failed to encode download request: %v", err), 0, nil)
	}

	endpoint := strings.TrimRight(cfg.Servers[0].URL, "/") + "/files/bulk-download"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(requestBody))
	if err != nil {
		return nil, errors.NewDaytonaError(fmt.Sprintf("Failed to create download request: %v", err), 0, nil)
	}

	for key, value := range cfg.DefaultHeader {
		req.Header.Set(key, value)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "multipart/form-data")

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, errors.NewDaytonaError(fmt.Sprintf("Failed to download file stream: %v", err), 0, nil)
	}

	if resp.StatusCode >= http.StatusMultipleChoices {
		defer resp.Body.Close()

		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return nil, errors.NewDaytonaError(fmt.Sprintf("Failed to read download error response: %v", readErr), resp.StatusCode, resp.Header)
		}

		return nil, errors.NewDaytonaErrorFromBody(body, resp.StatusCode, resp.Header)
	}

	_, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		_ = resp.Body.Close()
		return nil, errors.NewDaytonaError(fmt.Sprintf("Failed to parse multipart response: %v", err), resp.StatusCode, resp.Header)
	}

	boundary := params["boundary"]
	if boundary == "" {
		_ = resp.Body.Close()
		return nil, errors.NewDaytonaError("Missing multipart boundary in download response", resp.StatusCode, resp.Header)
	}

	reader := multipart.NewReader(resp.Body, boundary)
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			_ = resp.Body.Close()
			return nil, errors.NewDaytonaError("File stream not found in download response", resp.StatusCode, resp.Header)
		}
		if err != nil {
			_ = resp.Body.Close()
			return nil, errors.NewDaytonaError(fmt.Sprintf("Failed to read multipart download response: %v", err), resp.StatusCode, resp.Header)
		}

		switch part.FormName() {
		case "file":
			var reader io.Reader = part
			if onProgress != nil {
				reader = &progressReader{inner: part, onProgress: onProgress}
			}
			return &downloadStreamCloser{partReader: reader, response: resp}, nil
		case "error":
			body, readErr := io.ReadAll(part)
			_ = resp.Body.Close()
			if readErr != nil {
				return nil, errors.NewDaytonaError(fmt.Sprintf("Failed to read download error part: %v", readErr), resp.StatusCode, resp.Header)
			}
			return nil, errors.NewDaytonaErrorFromBody(body, resp.StatusCode, resp.Header)
		}
	}
}
