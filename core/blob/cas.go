// SPDX-License-Identifier: AGPL-3.0-or-later

// Package blob provides the local and explicitly configured remote
// content-addressed stores.
//
// The local implementation is filesystem-backed and sharded three levels deep:
//
//	$DATA_DIR/blobs/sha256/ab/cd/ef/abcdef...ff
//
// Hash-prefix sharding avoids collecting every blob in one directory.
// It distributes entries without imposing a fixed per-directory limit.
//
// Writes stream through sha256 and land via atomic rename from a
// per-put temp file at the CAS root (so the rename is
// same-device). Duplicate puts are cheap: same hash → same path → we
// keep the existing file and return the ref.
package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

// CAS is the content-addressed store facade.
type CAS struct {
	root     string // absolute path to blobs/sha256
	remote   RemoteBackend
	tempRoot string // dedicated private working directory for remote objects
}

// ErrNotFound is returned by Get/Stat when the requested hash is absent.
var ErrNotFound = errors.New("blob not found")

// New returns a CAS rooted at dir/blobs. Creates the shard root if
// missing. The blob directory is 0o750 — group readable so a sidecar
// container can serve /preview/ without needing root.
func New(dir string) (*CAS, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(abs, "blobs", "sha256")
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("mkdir cas root: %w", err)
	}
	return &CAS{root: root}, nil
}

// Put streams r into the store, returning the BlobRef. If the content
// already exists at its computed hash, Put is a no-op that still returns
// the ref — dedup is a property of the CAS, not the caller.
//
// The temp file is created at the CAS root so the
// final rename is always same-device (POSIX atomic).
func (c *CAS) Put(r io.Reader) (pluginapi.BlobRef, error) {
	return c.PutContext(context.Background(), r)
}

func installBlob(temporaryPath, destination string, size int64) error {
	if err := os.Rename(temporaryPath, destination); err != nil {
		// Windows does not replace an existing destination. Another Put may
		// have won after our preflight stat; matching size is the same dedup
		// condition used above.
		if info, statErr := os.Stat(destination); statErr == nil && info.Size() == size {
			return nil
		}
		return err
	}
	return nil
}

// Get opens a blob for reading.
func (c *CAS) Get(sum string) (io.ReadCloser, error) {
	return c.GetContext(context.Background(), sum)
}

// Path returns a blob's absolute filesystem path for rendered-view symlinks.
// It validates the hash but does not check whether the blob exists.
func (c *CAS) Path(sum string) (string, error) {
	if !validHash(sum) {
		return "", fmt.Errorf("bad hash %q", sum)
	}
	return c.path(sum), nil
}

// Stat returns the BlobRef for a hash if it exists.
func (c *CAS) Stat(sum string) (pluginapi.BlobRef, error) {
	return c.StatContext(context.Background(), sum)
}

// path returns the sharded path for a hash under the current
// three-level layout. Never called on user input without validHash
// first.
func (c *CAS) path(sum string) string {
	return filepath.Join(c.root, sum[0:2], sum[2:4], sum[4:6], sum)
}

// validHash checks that a string is exactly 64 lower-case hex chars.
// Anything else is suspicious — refuse to touch it.
func validHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
