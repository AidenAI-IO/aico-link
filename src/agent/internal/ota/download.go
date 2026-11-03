package ota

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"syscall"
	"time"

	"aico-agent/internal/logging"
)

const defaultProgressInterval = 5 * time.Second

var closeDownloadFile = func(f *os.File) error {
	return f.Close()
}

type DownloadOptions struct {
	BearerToken      string
	GitHubProxyURL   string
	Progress         func(DownloadProgress)
	ProgressInterval time.Duration
}

type DownloadProgress struct {
	URL         string
	Path        string
	Bytes       int64
	Total       int64
	ResumedFrom int64
	Complete    bool
}

func DownloadFile(ctx context.Context, url string, dst string, expectedSize int64) error {
	return DownloadFileWithOptions(ctx, url, dst, expectedSize, DownloadOptions{})
}

func DownloadFileWithToken(ctx context.Context, url string, dst string, expectedSize int64, bearerToken string) error {
	return DownloadFileWithOptions(ctx, url, dst, expectedSize, DownloadOptions{BearerToken: bearerToken})
}

func DownloadFileWithOptions(ctx context.Context, url string, dst string, expectedSize int64, options DownloadOptions) error {
	part := dst + ".part"
	resumeAt := int64(0)
	if info, err := os.Stat(part); err == nil {
		resumeAt = info.Size()
	} else if !os.IsNotExist(err) {
		return err
	}
	if expectedSize >= 0 {
		if resumeAt == expectedSize {
			if err := os.Rename(part, dst); err != nil {
				return err
			}
			if err := fsyncDirFor(dst); err != nil {
				return err
			}
			reportDownloadProgress(options.Progress, url, dst, resumeAt, expectedSize, resumeAt, true)
			return nil
		}
		if resumeAt > expectedSize {
			return fmt.Errorf("download size %d exceeds expected %d", resumeAt, expectedSize)
		}
	}

	// Apply GitHub proxy if configured
	downloadURL := ApplyGitHubProxy(url, options.GitHubProxyURL)
	if downloadURL != url && resumeAt == 0 {
		_ = logging.LogEvent(logging.Info, "ota", "download", "proxy_enabled")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return err
	}
	if options.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+options.BearerToken)
	}
	if resumeAt > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", resumeAt))
	}
	resp, err := defaultOTAHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("download status %d", resp.StatusCode)
	}

	flags := os.O_CREATE | os.O_WRONLY
	if resumeAt > 0 && resp.StatusCode == http.StatusPartialContent {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
		resumeAt = 0
	}
	reporter := newDownloadProgressReporter(options.Progress, url, dst, expectedSize, resumeAt, options.ProgressInterval)
	f, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return err
	}
	copyErr := error(nil)
	var copied int64
	body := &cumulativeProgressReader{
		reader: resp.Body,
		onRead: func(n int64) {
			reporter.maybeReport(resumeAt + n)
		},
	}
	if expectedSize >= 0 {
		remaining := expectedSize - resumeAt
		copied, copyErr = io.Copy(f, io.LimitReader(body, remaining+1))
		if copyErr == nil && copied > remaining {
			copyErr = fmt.Errorf("download size exceeds expected %d", expectedSize)
		}
	} else {
		copied, copyErr = io.Copy(f, body)
	}
	if copyErr == nil {
		copyErr = f.Sync()
	}
	closeErr := closeDownloadFile(f)
	if closeErr != nil {
		handleDownloadError(part, closeErr)
	}
	if copyErr != nil {
		handleDownloadError(part, copyErr)
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}

	info, err := os.Stat(part)
	if err != nil {
		return err
	}
	if expectedSize >= 0 && info.Size() != expectedSize {
		return fmt.Errorf("download size %d, want %d", info.Size(), expectedSize)
	}
	if err := os.Rename(part, dst); err != nil {
		return err
	}
	if err := fsyncDirFor(dst); err != nil {
		return err
	}
	reporter.complete(info.Size())
	return nil
}

type cumulativeProgressReader struct {
	reader io.Reader
	onRead func(int64)
	read   int64
}

func (r *cumulativeProgressReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.read += int64(n)
		r.onRead(r.read)
	}
	return n, err
}

type downloadProgressReporter struct {
	progress    func(DownloadProgress)
	url         string
	path        string
	total       int64
	resumedFrom int64
	interval    time.Duration
	lastReport  time.Time
	nextPercent int64
}

func newDownloadProgressReporter(progress func(DownloadProgress), url string, path string, total int64, resumedFrom int64, interval time.Duration) *downloadProgressReporter {
	if interval <= 0 {
		interval = defaultProgressInterval
	}
	return &downloadProgressReporter{
		progress:    progress,
		url:         url,
		path:        path,
		total:       total,
		resumedFrom: resumedFrom,
		interval:    interval,
		lastReport:  time.Now(),
		nextPercent: 10,
	}
}

func (r *downloadProgressReporter) maybeReport(bytes int64) {
	if r.progress == nil {
		return
	}
	now := time.Now()
	shouldReport := now.Sub(r.lastReport) >= r.interval
	if r.total > 0 {
		percent := bytes * 100 / r.total
		if percent >= r.nextPercent && percent < 100 {
			shouldReport = true
			for r.nextPercent <= percent {
				r.nextPercent += 10
			}
		}
	}
	if !shouldReport {
		return
	}
	r.lastReport = now
	reportDownloadProgress(r.progress, r.url, r.path, bytes, r.total, r.resumedFrom, false)
}

func (r *downloadProgressReporter) complete(bytes int64) {
	reportDownloadProgress(r.progress, r.url, r.path, bytes, r.total, r.resumedFrom, true)
}

func reportDownloadProgress(progress func(DownloadProgress), url string, path string, bytes int64, total int64, resumedFrom int64, complete bool) {
	if progress == nil {
		return
	}
	progress(DownloadProgress{
		URL:         url,
		Path:        path,
		Bytes:       bytes,
		Total:       total,
		ResumedFrom: resumedFrom,
		Complete:    complete,
	})
}

// handleDownloadError handles download errors, specifically cleaning up .part files on ENOSPC.
// For ENOSPC (no space left on device), the partial file is deleted because resume would fail
// at the same point. For other errors (network, timeout), the .part file is preserved for resume.
func handleDownloadError(partPath string, err error) {
	if err == nil {
		return
	}

	// Check if error is ENOSPC
	if errors.Is(err, syscall.ENOSPC) {
		// Delete .part file - resume would fail anyway
		if removeErr := os.Remove(partPath); removeErr != nil && !os.IsNotExist(removeErr) {
			_ = logging.LogEvent(logging.Warn, "ota", "download", "partial_remove_failed",
				logging.Field{Key: "path", Value: partPath},
				logging.Field{Key: "reason", Value: "enospc"},
				logging.Field{Key: "error", Value: removeErr},
			)
		} else {
			_ = logging.LogEvent(logging.Info, "ota", "download", "partial_removed",
				logging.Field{Key: "path", Value: partPath},
				logging.Field{Key: "reason", Value: "enospc"},
			)
		}
	}
	// For all other errors, preserve .part file for resume (existing behavior)
}
