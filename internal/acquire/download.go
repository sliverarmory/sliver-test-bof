package acquire

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	downloadAttempts = 3
	retryBaseDelay   = 250 * time.Millisecond
)

type httpStatusError struct {
	url        string
	statusCode int
	status     string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("download %s: HTTP %s", e.url, e.status)
}

func httpClient(client *http.Client) *http.Client {
	if client != nil {
		return client
	}
	return &http.Client{Timeout: 15 * time.Minute}
}

func downloadBytes(ctx context.Context, client *http.Client, rawURL, token string, limit int64) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < downloadAttempts; attempt++ {
		data, err := downloadBytesOnce(ctx, client, rawURL, token, limit)
		if err == nil {
			return data, nil
		}
		lastErr = err
		if !retryable(err) || attempt == downloadAttempts-1 {
			break
		}
		if err := waitRetry(ctx, retryBaseDelay<<attempt); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

func downloadBytesOnce(ctx context.Context, client *http.Client, rawURL, token string, limit int64) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		return nil, err
	}
	setDownloadHeaders(request, token)
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", rawURL, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		return nil, &httpStatusError{url: rawURL, statusCode: response.StatusCode, status: response.Status}
	}
	if limit <= 0 {
		return nil, errors.New("download limit must be positive")
	}
	if response.ContentLength > limit {
		return nil, fmt.Errorf("download %s is %d bytes, limit is %d", rawURL, response.ContentLength, limit)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", rawURL, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("download %s exceeded %d bytes", rawURL, limit)
	}
	return data, nil
}

func downloadFile(ctx context.Context, client *http.Client, rawURL, token, directory string, expectedSize, limit int64) (string, string, error) {
	var lastErr error
	for attempt := 0; attempt < downloadAttempts; attempt++ {
		path, digest, err := downloadFileOnce(ctx, client, rawURL, token, directory, expectedSize, limit)
		if err == nil {
			return path, digest, nil
		}
		lastErr = err
		if !retryable(err) || attempt == downloadAttempts-1 {
			break
		}
		if err := waitRetry(ctx, retryBaseDelay<<attempt); err != nil {
			return "", "", err
		}
	}
	return "", "", lastErr
}

func downloadFileOnce(ctx context.Context, client *http.Client, rawURL, token, directory string, expectedSize, limit int64) (string, string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		return "", "", err
	}
	setDownloadHeaders(request, token)
	response, err := client.Do(request)
	if err != nil {
		return "", "", fmt.Errorf("download %s: %w", rawURL, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		return "", "", &httpStatusError{url: rawURL, statusCode: response.StatusCode, status: response.Status}
	}
	if limit <= 0 {
		return "", "", errors.New("download limit must be positive")
	}
	if response.ContentLength > limit {
		return "", "", fmt.Errorf("download %s is %d bytes, limit is %d", rawURL, response.ContentLength, limit)
	}
	if expectedSize > 0 && response.ContentLength >= 0 && response.ContentLength != expectedSize {
		return "", "", fmt.Errorf("download %s content length %d, expected %d", rawURL, response.ContentLength, expectedSize)
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", "", err
	}
	prefix := filepath.Base(parsed.Path)
	if prefix == "." || prefix == string(filepath.Separator) || prefix == "" {
		prefix = "download"
	}
	temporary, err := os.CreateTemp(directory, prefix+"-*")
	if err != nil {
		return "", "", err
	}
	path := temporary.Name()
	success := false
	defer func() {
		_ = temporary.Close()
		if !success {
			_ = os.Remove(path)
		}
	}()
	hash := sha256.New()
	reader := io.LimitReader(response.Body, limit+1)
	written, err := io.Copy(io.MultiWriter(temporary, hash), reader)
	if err != nil {
		return "", "", fmt.Errorf("write %s: %w", rawURL, err)
	}
	if written > limit {
		return "", "", fmt.Errorf("download %s exceeded %d bytes", rawURL, limit)
	}
	if expectedSize > 0 && written != expectedSize {
		return "", "", fmt.Errorf("download %s wrote %d bytes, expected %d", rawURL, written, expectedSize)
	}
	if err := temporary.Sync(); err != nil {
		return "", "", err
	}
	if err := temporary.Close(); err != nil {
		return "", "", err
	}
	success = true
	return path, hex.EncodeToString(hash.Sum(nil)), nil
}

func setDownloadHeaders(request *http.Request, token string) {
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "sliver-bof-e2e-action")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if strings.TrimSpace(token) != "" {
		request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	}
}

func retryable(err error) bool {
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var status *httpStatusError
	if errors.As(err, &status) {
		return status.statusCode == http.StatusTooManyRequests || status.statusCode >= 500 && status.statusCode <= 599
	}
	var network net.Error
	return errors.As(err, &network) && (network.Timeout() || network.Temporary())
}

func waitRetry(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func sha256File(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func sha256Bytes(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func trustedCommentFile(comment string) string {
	for _, field := range strings.Fields(comment) {
		if strings.HasPrefix(field, "file:") {
			return strings.TrimPrefix(field, "file:")
		}
	}
	return ""
}
