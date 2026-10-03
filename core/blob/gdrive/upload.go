// SPDX-License-Identifier: AGPL-3.0-or-later

package gdrive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/blob"
)

const (
	uploadChunkSize     = 8 << 20
	maxUploadRetries    = 4
	maxSessionRestarts  = 2
	maxRetryDelay       = 2 * time.Second
	initialRetryDelay   = 100 * time.Millisecond
	resumableUploadType = "resumable"
	uploadMIMEType      = "application/octet-stream"
)

var _ blob.RemoteBackend = (*Store)(nil)

// Put validates the requested hash and size, then creates the immutable object
// through a resumable Drive upload.
func (s *Store) Put(ctx context.Context, sum string, r io.ReadSeeker, size int64) (retErr error) {
	s.putMu.Lock()
	defer s.putMu.Unlock()

	if !validHash(sum) {
		return fmt.Errorf("invalid SHA-256 hash %q", sum)
	}
	if size < 0 {
		return errors.New("Google Drive blob size cannot be negative")
	}
	if r == nil {
		return errors.New("Google Drive blob input is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	initialOffset, err := r.Seek(0, io.SeekCurrent)
	if err != nil || initialOffset < 0 {
		return errors.New("Google Drive blob input must support seeking")
	}
	defer func() {
		if _, err := r.Seek(initialOffset, io.SeekStart); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("restore Google Drive blob input position: %w", err))
		}
	}()
	if initialOffset > int64(^uint64(0)>>1)-size {
		return errors.New("Google Drive blob input offset and size overflow")
	}

	hash := sha256.New()
	written, err := copyAndHash(ctx, io.Discard, r, hash)
	if err != nil {
		return fmt.Errorf("verify Google Drive blob input: %w", err)
	}
	if written != size {
		return fmt.Errorf("Google Drive blob size mismatch: input has %d bytes, caller supplied %d", written, size)
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != sum {
		return errors.New("Google Drive blob input failed SHA-256 verification")
	}
	if _, err := r.Seek(initialOffset, io.SeekStart); err != nil {
		return fmt.Errorf("rewind Google Drive blob input: %w", err)
	}

	file, found, err := s.lookupBlob(ctx, sum)
	if err != nil {
		return err
	}
	if found {
		return s.verifyExisting(ctx, file, sum, size)
	}

	for restart := 0; restart <= maxSessionRestarts; restart++ {
		session, err := s.startResumableUpload(ctx, sum, size)
		if err != nil {
			return err
		}
		complete, expired, transferErr := s.transferUpload(ctx, session, r, initialOffset, size)
		if transferErr != nil {
			// A lost final response can mean Drive accepted the bytes. Reconcile by
			// querying the owned hash before returning the ambiguous transfer error.
			file, found, lookupErr := s.lookupBlob(ctx, sum)
			if lookupErr == nil && found {
				return s.verifyExisting(ctx, file, sum, size)
			}
			if lookupErr != nil {
				return errors.Join(transferErr, fmt.Errorf("reconcile Google Drive upload: %w", lookupErr))
			}
			return transferErr
		}
		if expired {
			file, found, lookupErr := s.lookupBlob(ctx, sum)
			if lookupErr != nil {
				return fmt.Errorf("reconcile expired Google Drive upload: %w", lookupErr)
			}
			if found {
				return s.verifyExisting(ctx, file, sum, size)
			}
			continue
		}
		if !complete {
			return errors.New("Google Drive resumable upload ended without completion")
		}
		return s.verifyUploaded(ctx, sum, size)
	}
	return errors.New("Google Drive resumable upload session repeatedly expired")
}

func (s *Store) verifyUploaded(ctx context.Context, sum string, size int64) error {
	file, found, err := s.lookupBlob(ctx, sum)
	if err != nil {
		return fmt.Errorf("verify completed Google Drive upload: %w", err)
	}
	if !found {
		return errors.New("Google Drive reported upload completion but the blob is missing")
	}
	return s.verifyExisting(ctx, file, sum, size)
}

func (s *Store) verifyExisting(ctx context.Context, file driveFile, sum string, size int64) error {
	if err := s.validateBlobFile(sum, file); err != nil {
		return err
	}
	actualSize, err := file.size()
	if err != nil {
		return err
	}
	if actualSize != size {
		return fmt.Errorf("immutable Google Drive blob size mismatch: stored %d bytes, requested %d", actualSize, size)
	}
	if file.SHA256 != "" {
		return nil
	}
	reader, err := s.Get(ctx, sum)
	if err != nil {
		return fmt.Errorf("verify Google Drive blob without provider checksum: %w", err)
	}
	hash := sha256.New()
	readSize, copyErr := copyAndHash(ctx, io.Discard, reader, hash)
	closeErr := reader.Close()
	if copyErr != nil {
		return fmt.Errorf("read back Google Drive blob: %w", copyErr)
	}
	if readSize != size || hex.EncodeToString(hash.Sum(nil)) != sum {
		return errors.New("Google Drive blob readback failed size or SHA-256 verification")
	}
	if closeErr != nil {
		return fmt.Errorf("close Google Drive blob readback: %w", closeErr)
	}
	return nil
}

func (s *Store) startResumableUpload(ctx context.Context, sum string, size int64) (string, error) {
	metadata := map[string]any{
		"name":     sum,
		"mimeType": uploadMIMEType,
		"parents":  []string{s.namespaceID},
		"appProperties": map[string]string{
			blobProperty: sum,
		},
	}
	body, err := json.Marshal(metadata)
	if err != nil {
		return "", errors.New("encode Google Drive upload metadata")
	}
	query := url.Values{
		"uploadType":        []string{resumableUploadType},
		"supportsAllDrives": []string{"true"},
		"fields":            []string{"id,name,size,sha256Checksum,modifiedTime,parents,appProperties"},
	}
	target := s.drive.uploadURL("/files", query)
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json; charset=UTF-8")
	headers.Set("X-Upload-Content-Type", uploadMIMEType)
	headers.Set("X-Upload-Content-Length", strconv.FormatInt(size, 10))

	for attempt := 0; ; attempt++ {
		resp, requestErr := s.drive.do(ctx, http.MethodPost, target, bytes.NewReader(body), headers)
		if requestErr != nil {
			if !retryableTransport(requestErr) || attempt >= maxUploadRetries {
				return "", fmt.Errorf("start Google Drive resumable upload failed")
			}
			if err := waitForRetry(ctx, retryDelay(attempt, "")); err != nil {
				return "", err
			}
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			location := resp.Header.Get("Location")
			_ = resp.Body.Close()
			validated, err := s.drive.validateUploadLocation(location)
			if err != nil {
				return "", err
			}
			return validated, nil
		}
		retryAfter := resp.Header.Get("Retry-After")
		status := resp.StatusCode
		_ = resp.Body.Close()
		if !retryableStatus(status) || attempt >= maxUploadRetries {
			return "", httpStatusError(status)
		}
		if err := waitForRetry(ctx, retryDelay(attempt, retryAfter)); err != nil {
			return "", err
		}
	}
}

func (d *driveClient) validateUploadLocation(location string) (string, error) {
	u, err := url.Parse(location)
	if err != nil || !u.IsAbs() || u.Host == "" || u.User != nil || u.Fragment != "" {
		return "", errors.New("Google Drive returned an invalid resumable upload URL")
	}
	if !sameOrigin(d.uploadBase, u) {
		return "", errors.New("Google Drive returned an off-origin resumable upload URL")
	}
	if u.Scheme != "https" && !isLoopbackHost(u.Hostname()) {
		return "", errors.New("Google Drive returned an insecure resumable upload URL")
	}
	return u.String(), nil
}

func (s *Store) transferUpload(ctx context.Context, session string, source io.ReadSeeker, sourceOffset, size int64) (complete, expired bool, err error) {
	var offset int64
	retries := 0
	for {
		if err := ctx.Err(); err != nil {
			return false, false, err
		}
		if offset == size && size > 0 {
			next, done, sessionExpired, statusErr := s.queryUploadStatus(ctx, session, size)
			if statusErr != nil {
				return false, false, statusErr
			}
			if done {
				return true, false, nil
			}
			if sessionExpired {
				return false, true, nil
			}
			if next < size {
				offset = next
				retries = 0
				continue
			}
			return false, false, errors.New("Google Drive acknowledged all upload bytes without completing the session")
		}

		end := offset + uploadChunkSize
		if size == 0 {
			end = 0
		} else if end > size {
			end = size
		}
		data := make([]byte, int(end-offset))
		if len(data) > 0 {
			if _, err := source.Seek(sourceOffset+offset, io.SeekStart); err != nil {
				return false, false, fmt.Errorf("seek Google Drive upload spool: %w", err)
			}
			n, readErr := io.ReadFull(source, data)
			if readErr != nil || n != len(data) {
				return false, false, errors.New("Google Drive upload spool ended before the declared size")
			}
		}
		headers := make(http.Header)
		headers.Set("Content-Length", strconv.Itoa(len(data)))
		if size > 0 {
			headers.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, end-1, size))
		}
		resp, requestErr := s.drive.do(ctx, http.MethodPut, session, bytes.NewReader(data), headers)
		if requestErr != nil {
			if !retryableTransport(requestErr) {
				return false, false, requestErr
			}
			next, done, sessionExpired, statusErr := s.queryUploadStatus(ctx, session, size)
			if statusErr != nil {
				return false, false, errors.Join(requestErr, statusErr)
			}
			if done {
				return true, false, nil
			}
			if sessionExpired {
				return false, true, nil
			}
			if next > offset {
				offset = next
				retries = 0
				continue
			}
			if retries >= maxUploadRetries {
				return false, false, errors.New("Google Drive upload did not advance after repeated transport failures")
			}
			if err := waitForRetry(ctx, retryDelay(retries, "")); err != nil {
				return false, false, err
			}
			retries++
			continue
		}

		switch resp.StatusCode {
		case http.StatusOK, http.StatusCreated:
			_ = resp.Body.Close()
			return true, false, nil
		case 308:
			next, parseErr := acknowledgedOffset(resp.Header.Get("Range"), size)
			_ = resp.Body.Close()
			if parseErr != nil {
				return false, false, parseErr
			}
			if next < offset {
				return false, false, errors.New("Google Drive upload acknowledgement moved backwards")
			}
			if next > offset {
				offset = next
				retries = 0
				continue
			}
			if retries >= maxUploadRetries {
				return false, false, errors.New("Google Drive upload made no progress after repeated acknowledgements")
			}
			if err := waitForRetry(ctx, retryDelay(retries, "")); err != nil {
				return false, false, err
			}
			retries++
		case http.StatusNotFound:
			_ = resp.Body.Close()
			return false, true, nil
		default:
			retryAfter := resp.Header.Get("Retry-After")
			status := resp.StatusCode
			_ = resp.Body.Close()
			if !retryableStatus(status) {
				return false, false, httpStatusError(status)
			}
			next, done, sessionExpired, statusErr := s.queryUploadStatus(ctx, session, size)
			if statusErr != nil {
				return false, false, statusErr
			}
			if done {
				return true, false, nil
			}
			if sessionExpired {
				return false, true, nil
			}
			if next > offset {
				offset = next
				retries = 0
				continue
			}
			if retries >= maxUploadRetries {
				return false, false, httpStatusError(status)
			}
			if err := waitForRetry(ctx, retryDelay(retries, retryAfter)); err != nil {
				return false, false, err
			}
			retries++
		}
	}
}

func (s *Store) queryUploadStatus(ctx context.Context, session string, size int64) (offset int64, complete, expired bool, err error) {
	headers := make(http.Header)
	headers.Set("Content-Length", "0")
	headers.Set("Content-Range", fmt.Sprintf("bytes */%d", size))
	for attempt := 0; ; attempt++ {
		resp, requestErr := s.drive.do(ctx, http.MethodPut, session, bytes.NewReader(nil), headers)
		if requestErr != nil {
			if !retryableTransport(requestErr) || attempt >= maxUploadRetries {
				return 0, false, false, requestErr
			}
			if err := waitForRetry(ctx, retryDelay(attempt, "")); err != nil {
				return 0, false, false, err
			}
			continue
		}
		switch resp.StatusCode {
		case http.StatusOK, http.StatusCreated:
			_ = resp.Body.Close()
			return size, true, false, nil
		case 308:
			offset, err := acknowledgedOffset(resp.Header.Get("Range"), size)
			_ = resp.Body.Close()
			return offset, false, false, err
		case http.StatusNotFound:
			_ = resp.Body.Close()
			return 0, false, true, nil
		default:
			retryAfter := resp.Header.Get("Retry-After")
			status := resp.StatusCode
			_ = resp.Body.Close()
			if !retryableStatus(status) || attempt >= maxUploadRetries {
				return 0, false, false, httpStatusError(status)
			}
			if err := waitForRetry(ctx, retryDelay(attempt, retryAfter)); err != nil {
				return 0, false, false, err
			}
		}
	}
}

func acknowledgedOffset(header string, size int64) (int64, error) {
	if header == "" {
		return 0, nil
	}
	if !strings.HasPrefix(header, "bytes=0-") {
		return 0, errors.New("Google Drive returned an invalid upload Range header")
	}
	end, err := strconv.ParseInt(strings.TrimPrefix(header, "bytes=0-"), 10, 64)
	if err != nil || end < 0 || end >= size {
		return 0, errors.New("Google Drive returned an out-of-range upload acknowledgement")
	}
	return end + 1, nil
}

func retryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusInternalServerError ||
		status == http.StatusBadGateway || status == http.StatusServiceUnavailable ||
		status == http.StatusGatewayTimeout
}

func retryableTransport(err error) bool {
	return errors.Is(err, errTransport)
}

func retryDelay(attempt int, retryAfter string) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && seconds >= 0 {
		if seconds == 0 {
			return 0
		}
		if seconds >= int(maxRetryDelay/time.Second) {
			return maxRetryDelay
		}
		return time.Duration(seconds) * time.Second
	}
	if retryTime, err := http.ParseTime(retryAfter); err == nil {
		delay := time.Until(retryTime)
		if delay > 0 {
			if delay > maxRetryDelay {
				return maxRetryDelay
			}
			return delay
		}
	}
	delay := initialRetryDelay << min(attempt, 4)
	if delay > maxRetryDelay {
		return maxRetryDelay
	}
	return delay
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
