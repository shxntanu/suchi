// SPDX-License-Identifier: AGPL-3.0-or-later

package blob

import (
	"context"
	"io"
	"time"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

// RemoteObject describes an immutable stored blob and its provider modification time.
type RemoteObject struct {
	Ref      pluginapi.BlobRef
	Modified time.Time
}

// RemoteBackend persists immutable SHA-256 objects in an owned remote namespace.
// Put must confirm the persisted size and SHA-256 before success, using a
// trusted provider checksum or a full readback. It must never replace bytes at
// an existing hash. Get returns a stream; the CAS verifies it before exposure.
// Missing objects return ErrNotFound.
type RemoteBackend interface {
	Put(context.Context, string, io.ReadSeeker, int64) error
	Get(context.Context, string) (io.ReadCloser, error)
	Stat(context.Context, string) (RemoteObject, error)
	List(context.Context, func(RemoteObject) error) error
	Delete(context.Context, string) error
}
