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
// Put must verify persistence before success; missing objects return ErrNotFound.
type RemoteBackend interface {
	Put(context.Context, string, io.ReadSeeker, int64) error
	Get(context.Context, string) (io.ReadCloser, error)
	Stat(context.Context, string) (RemoteObject, error)
	List(context.Context, func(RemoteObject) error) error
	Delete(context.Context, string) error
}
