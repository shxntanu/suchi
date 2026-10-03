// SPDX-License-Identifier: AGPL-3.0-or-later

package gdrive

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/johnnybravo-xyz/suchi/core/blob"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

// Stat returns the metadata for an owned blob without downloading its bytes.
func (s *Store) Stat(ctx context.Context, sum string) (blob.RemoteObject, error) {
	if !validHash(sum) {
		return blob.RemoteObject{}, fmt.Errorf("invalid SHA-256 hash %q", sum)
	}
	file, found, err := s.lookupBlob(ctx, sum)
	if err != nil {
		return blob.RemoteObject{}, err
	}
	if !found {
		return blob.RemoteObject{}, blob.ErrNotFound
	}
	return remoteObject(file, sum)
}

// Get streams one owned blob from Drive. The CAS facade hashes and stages the
// response before exposing a seekable local reader to callers.
func (s *Store) Get(ctx context.Context, sum string) (io.ReadCloser, error) {
	if !validHash(sum) {
		return nil, fmt.Errorf("invalid SHA-256 hash %q", sum)
	}
	file, found, err := s.lookupBlob(ctx, sum)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, blob.ErrNotFound
	}
	query := url.Values{
		"alt":               []string{"media"},
		"supportsAllDrives": []string{"true"},
	}
	resp, err := s.drive.request(ctx, http.MethodGet, s.drive.apiFileURL(file.ID, query), nil, nil)
	if err != nil {
		return nil, fmt.Errorf("download Google Drive blob: %w", err)
	}
	// The caller owns the successful response body. CAS closes it after staging
	// and verifying the bytes, and readback verification closes it directly.
	return resp.Body, nil
}

// List visits all blobs carrying Suchi's hash marker under the marked folder.
func (s *Store) List(ctx context.Context, fn func(blob.RemoteObject) error) error {
	if fn == nil {
		return errors.New("Google Drive blob list callback is nil")
	}
	files, err := s.ownedBlobs(ctx)
	if err != nil {
		return fmt.Errorf("list owned Google Drive blobs: %w", err)
	}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		sum := file.AppProperties[blobProperty]
		object, err := remoteObject(file, sum)
		if err != nil {
			return err
		}
		if err := fn(object); err != nil {
			return err
		}
	}
	return nil
}

// Delete removes one blob from Suchi's namespace. Deleting a missing blob is
// idempotent.
func (s *Store) Delete(ctx context.Context, sum string) error {
	if !validHash(sum) {
		return fmt.Errorf("invalid SHA-256 hash %q", sum)
	}
	file, found, err := s.lookupBlob(ctx, sum)
	if err != nil || !found {
		return err
	}
	query := url.Values{"supportsAllDrives": []string{"true"}}
	resp, err := s.drive.request(ctx, http.MethodDelete, s.drive.apiFileURL(file.ID, query), nil, nil)
	if err == nil {
		_ = resp.Body.Close()
		return nil
	}
	var statusErr apiStatusError
	if errors.As(err, &statusErr) && statusErr.status == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("delete Google Drive blob: %w", err)
}

func remoteObject(file driveFile, sum string) (blob.RemoteObject, error) {
	size, err := file.size()
	if err != nil {
		return blob.RemoteObject{}, err
	}
	modified, err := file.modified()
	if err != nil {
		return blob.RemoteObject{}, err
	}
	return blob.RemoteObject{
		Ref:      pluginapi.BlobRef{SHA256: sum, Size: size},
		Modified: modified,
	}, nil
}

func checksumMatches(checksum, sum string) bool {
	checksum = strings.TrimSpace(checksum)
	if strings.EqualFold(checksum, sum) {
		return true
	}
	rawHex, err := hex.DecodeString(checksum)
	if err == nil && hex.EncodeToString(rawHex) == sum {
		return true
	}
	decoders := []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	}
	for _, decoder := range decoders {
		raw, err := decoder.DecodeString(checksum)
		if err == nil && hex.EncodeToString(raw) == sum {
			return true
		}
	}
	return false
}

func copyAndHash(ctx context.Context, dst io.Writer, src io.Reader, hash io.Writer) (int64, error) {
	buf := make([]byte, 128*1024)
	var total int64
	emptyReads := 0
	out := io.MultiWriter(dst, hash)
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			emptyReads = 0
			written, writeErr := out.Write(buf[:n])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
			if written != n {
				return total, io.ErrShortWrite
			}
		}
		if n == 0 && readErr == nil {
			emptyReads++
			if emptyReads >= 100 {
				return total, io.ErrNoProgress
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return total, nil
			}
			return total, readErr
		}
	}
}
