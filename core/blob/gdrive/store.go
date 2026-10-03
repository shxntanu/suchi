// SPDX-License-Identifier: AGPL-3.0-or-later

package gdrive

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"

	"github.com/johnnybravo-xyz/suchi/core/blob"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

var _ interface {
	Get(context.Context, string) (io.ReadCloser, error)
	Stat(context.Context, string) (blob.RemoteObject, error)
	List(context.Context, func(blob.RemoteObject) error) error
	Delete(context.Context, string) error
} = (*Store)(nil)

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

// Get downloads, hashes, and rewinds one owned blob into a private temporary
// file. Closing the reader removes the temporary file.
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
	query := url.Values{"alt": []string{"media"}}
	resp, err := s.drive.request(ctx, http.MethodGet, s.drive.apiURL(fileURLPath(file.ID), query), nil, nil)
	if err != nil {
		return nil, fmt.Errorf("download Google Drive blob: %w", err)
	}
	defer resp.Body.Close()

	tmp, err := os.CreateTemp("", "suchi-gdrive-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("create private Google Drive download file: %w", err)
	}
	path := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(path)
	}
	expectedSize, err := file.size()
	if err != nil {
		cleanup()
		return nil, err
	}
	hash := sha256.New()
	size, err := copyAndHash(ctx, tmp, resp.Body, hash)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("download Google Drive blob bytes: %w", err)
	}
	if size != expectedSize {
		cleanup()
		return nil, fmt.Errorf("Google Drive blob size mismatch: got %d bytes, expected %d", size, expectedSize)
	}
	gotSum := hex.EncodeToString(hash.Sum(nil))
	if gotSum != sum {
		cleanup()
		return nil, errors.New("Google Drive blob content failed SHA-256 verification")
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, fmt.Errorf("rewind verified Google Drive blob: %w", err)
	}
	return &tempReader{file: tmp, path: path}, nil
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
	resp, err := s.drive.request(ctx, http.MethodDelete, s.drive.apiURL(fileURLPath(file.ID), nil), nil, nil)
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
	out := io.MultiWriter(dst, hash)
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			written, writeErr := out.Write(buf[:n])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
			if written != n {
				return total, io.ErrShortWrite
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

type tempReader struct {
	file *os.File
	path string
	once sync.Once
	err  error
}

func (r *tempReader) Read(p []byte) (int, error) {
	return r.file.Read(p)
}

func (r *tempReader) Seek(offset int64, whence int) (int64, error) {
	return r.file.Seek(offset, whence)
}

func (r *tempReader) Close() error {
	r.once.Do(func() {
		closeErr := r.file.Close()
		removeErr := os.Remove(r.path)
		if closeErr != nil {
			r.err = closeErr
		} else if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			r.err = removeErr
		}
	})
	return r.err
}
