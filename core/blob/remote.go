// SPDX-License-Identifier: AGPL-3.0-or-later

package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

const (
	remoteTempDirName = ".suchi-remote-cas-tmp"
	remoteTempOwner   = ".owner"
	remoteTempMarker  = "suchi-remote-cas-temp-v1\n"
)

var remoteTempInitMu sync.Mutex

var (
	// ErrIntegrity reports remote or materialized bytes that do not match their CAS key.
	ErrIntegrity = errors.New("blob integrity check failed")
	// ErrRemoteQuarantineUnsupported marks remote scrub repair as deliberately unsupported.
	ErrRemoteQuarantineUnsupported = errors.New("quarantine is unsupported for remote blob storage")
)

// NewRemote returns a CAS facade backed by an immutable remote object store.
// Its private temporary directory is separate from the canonical local blob tree.
func NewRemote(dir string, backend RemoteBackend) (*CAS, error) {
	if backend == nil {
		return nil, errors.New("remote blob backend is required")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return nil, fmt.Errorf("mkdir remote CAS directory: %w", err)
	}
	remoteTempInitMu.Lock()
	defer remoteTempInitMu.Unlock()
	tempBase := filepath.Join(abs, remoteTempDirName)
	if err := ensureRemoteTempRoot(tempBase); err != nil {
		return nil, err
	}
	tempRoot, err := newRemoteTempSession(tempBase)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(abs, "blobs", "sha256")
	return &CAS{root: root, remote: backend, tempRoot: tempRoot}, nil
}

func ensureRemoteTempRoot(path string) error {
	created := false
	if err := os.Mkdir(path, 0o700); err == nil {
		created = true
	} else if !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("create remote CAS temp directory: %w", err)
	}

	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect remote CAS temp directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("remote CAS temp path is not a real directory: %s", path)
	}

	markerPath := filepath.Join(path, remoteTempOwner)
	if created {
		marker, err := os.OpenFile(markerPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			_ = os.Remove(path)
			return fmt.Errorf("create remote CAS temp ownership marker: %w", err)
		}
		_, writeErr := io.WriteString(marker, remoteTempMarker)
		syncErr := marker.Sync()
		closeErr := marker.Close()
		if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
			_ = os.Remove(markerPath)
			_ = os.Remove(path)
			return fmt.Errorf("write remote CAS temp ownership marker: %w", err)
		}
	} else {
		markerInfo, err := os.Lstat(markerPath)
		if err != nil {
			return fmt.Errorf("remote CAS temp directory is not owned by Suchi: %w", err)
		}
		if !markerInfo.Mode().IsRegular() || markerInfo.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("remote CAS temp ownership marker is not a regular file: %s", markerPath)
		}
		contents, err := os.ReadFile(markerPath)
		if err != nil {
			return fmt.Errorf("read remote CAS temp ownership marker: %w", err)
		}
		if string(contents) != remoteTempMarker {
			return fmt.Errorf("remote CAS temp ownership marker is invalid: %s", markerPath)
		}
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("secure remote CAS temp directory: %w", err)
	}
	if err := os.Chmod(markerPath, 0o600); err != nil {
		return fmt.Errorf("secure remote CAS temp ownership marker: %w", err)
	}
	return nil
}

func newRemoteTempSession(tempBase string) (string, error) {
	path, err := os.MkdirTemp(tempBase, "session-")
	if err != nil {
		return "", fmt.Errorf("create remote CAS temp session: %w", err)
	}
	marker, err := os.OpenFile(filepath.Join(path, remoteTempOwner), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("create remote CAS session marker: %w", err)
	}
	_, writeErr := io.WriteString(marker, remoteTempMarker)
	syncErr := marker.Sync()
	closeErr := marker.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		_ = os.Remove(filepath.Join(path, remoteTempOwner))
		_ = os.Remove(path)
		return "", fmt.Errorf("write remote CAS session marker: %w", err)
	}
	return path, nil
}

// CleanupRemoteTempFiles removes recognized working files left in marked
// remote-CAS sessions. Call only during offline startup, before opening any
// remote CAS for this archive. NewRemote deliberately does not sweep shared
// session directories because other server instances or tools may be reading.
func CleanupRemoteTempFiles(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	remoteTempInitMu.Lock()
	defer remoteTempInitMu.Unlock()
	tempBase := filepath.Join(abs, remoteTempDirName)
	if _, err := os.Lstat(tempBase); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect remote CAS temp root: %w", err)
	}
	if err := validateOwnedTempDir(tempBase); err != nil {
		return err
	}
	entries, err := os.ReadDir(tempBase)
	if err != nil {
		return fmt.Errorf("read remote CAS temp root: %w", err)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "session-") {
			continue
		}
		path := filepath.Join(tempBase, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("inspect abandoned remote CAS session %q: %w", entry.Name(), err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if err := validateOwnedTempDir(path); err != nil {
			return err
		}
		if err := cleanupRemoteSession(path); err != nil {
			return err
		}
	}
	return nil
}

func cleanupRemoteSession(path string) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		return fmt.Errorf("read remote CAS temp session: %w", err)
	}
	for _, entry := range entries {
		if !ownedTempName(entry.Name()) {
			continue
		}
		tempPath := filepath.Join(path, entry.Name())
		info, err := os.Lstat(tempPath)
		if err != nil {
			return fmt.Errorf("inspect abandoned remote CAS temp %q: %w", entry.Name(), err)
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if err := os.Remove(tempPath); err != nil {
			return fmt.Errorf("remove abandoned remote CAS temp %q: %w", entry.Name(), err)
		}
	}
	entries, err = os.ReadDir(path)
	if err != nil {
		return fmt.Errorf("recheck remote CAS temp session: %w", err)
	}
	if len(entries) == 1 && entries[0].Name() == remoteTempOwner {
		if err := os.Remove(filepath.Join(path, remoteTempOwner)); err != nil {
			return fmt.Errorf("remove remote CAS session marker: %w", err)
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove empty remote CAS temp session: %w", err)
		}
	}
	return nil
}

func validateOwnedTempDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect remote CAS temp directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("remote CAS temp path is not a real directory: %s", path)
	}
	markerPath := filepath.Join(path, remoteTempOwner)
	markerInfo, err := os.Lstat(markerPath)
	if err != nil {
		return fmt.Errorf("remote CAS temp directory is not owned by Suchi: %w", err)
	}
	if !markerInfo.Mode().IsRegular() || markerInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("remote CAS temp ownership marker is not a regular file: %s", markerPath)
	}
	contents, err := os.ReadFile(markerPath)
	if err != nil {
		return fmt.Errorf("read remote CAS temp ownership marker: %w", err)
	}
	if string(contents) != remoteTempMarker {
		return fmt.Errorf("remote CAS temp ownership marker is invalid: %s", markerPath)
	}
	return nil
}

func ownedTempName(name string) bool {
	return (strings.HasPrefix(name, "upload-") || strings.HasPrefix(name, "download-")) && strings.HasSuffix(name, ".tmp")
}

// IsRemote reports whether this CAS persists blobs through a RemoteBackend.
func (c *CAS) IsRemote() bool { return c.remote != nil }

// PutContext writes content to the selected CAS backend. Remote bytes are
// staged locally and hashed before the backend can publish them.
func (c *CAS) PutContext(ctx context.Context, r io.Reader) (pluginapi.BlobRef, error) {
	if err := requireContext(ctx); err != nil {
		return pluginapi.BlobRef{}, err
	}
	if r == nil {
		return pluginapi.BlobRef{}, errors.New("blob reader is required")
	}
	if c.remote != nil {
		return c.putRemote(ctx, r)
	}
	return c.putLocalContext(ctx, r)
}

func (c *CAS) putLocalContext(ctx context.Context, r io.Reader) (ref pluginapi.BlobRef, retErr error) {
	tmp, err := os.CreateTemp(c.root, ".put-*.tmp")
	if err != nil {
		return pluginapi.BlobRef{}, err
	}
	tmpName := tmp.Name()
	tmpOpen := true
	defer func() {
		if tmpOpen {
			retErr = errors.Join(retErr, tmp.Close())
		}
		retErr = errors.Join(retErr, removeTemp(tmpName))
	}()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), contextReader{ctx: ctx, reader: r})
	if err != nil {
		return pluginapi.BlobRef{}, fmt.Errorf("stream: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return pluginapi.BlobRef{}, err
	}
	if err := tmp.Sync(); err != nil {
		return pluginapi.BlobRef{}, fmt.Errorf("fsync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		tmpOpen = false
		return pluginapi.BlobRef{}, err
	}
	tmpOpen = false

	sum := hex.EncodeToString(h.Sum(nil))
	dst := c.path(sum)
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return pluginapi.BlobRef{}, fmt.Errorf("mkdir shard: %w", err)
	}
	if fi, statErr := os.Stat(dst); statErr == nil && fi.Size() == n {
		return pluginapi.BlobRef{SHA256: sum, Size: n}, nil
	}
	if err := installBlob(tmp.Name(), dst, n); err != nil {
		return pluginapi.BlobRef{}, fmt.Errorf("rename: %w", err)
	}
	if err := os.Chmod(dst, 0o640); err != nil {
		return pluginapi.BlobRef{}, fmt.Errorf("chmod %s: %w", dst, err)
	}
	return pluginapi.BlobRef{SHA256: sum, Size: n}, nil
}

func (c *CAS) putRemote(ctx context.Context, r io.Reader) (ref pluginapi.BlobRef, retErr error) {
	tmp, err := os.CreateTemp(c.tempRoot, "upload-*.tmp")
	if err != nil {
		return pluginapi.BlobRef{}, fmt.Errorf("create remote upload temp: %w", err)
	}
	tmpName := tmp.Name()
	tmpOpen := true
	defer func() {
		if tmpOpen {
			retErr = errors.Join(retErr, tmp.Close())
		}
		retErr = errors.Join(retErr, removeTemp(tmpName))
	}()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), contextReader{ctx: ctx, reader: r})
	if err != nil {
		return pluginapi.BlobRef{}, fmt.Errorf("stage remote blob: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return pluginapi.BlobRef{}, err
	}
	if err := tmp.Sync(); err != nil {
		return pluginapi.BlobRef{}, fmt.Errorf("sync remote upload temp: %w", err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return pluginapi.BlobRef{}, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	want := pluginapi.BlobRef{SHA256: sum, Size: n}

	// An existing object at this key is immutable. Check its bytes before
	// treating it as a successful deduplicated publication.
	if existing, statErr := c.remote.Stat(ctx, sum); statErr == nil {
		if err := validateRemoteObject(sum, existing); err != nil {
			return pluginapi.BlobRef{}, err
		}
		if existing.Ref.Size != n {
			return pluginapi.BlobRef{}, fmt.Errorf("%w: remote object %s has size %d, input has size %d", ErrIntegrity, sum, existing.Ref.Size, n)
		}
		if err := c.verifyRemote(ctx, want); err != nil {
			return pluginapi.BlobRef{}, err
		}
		return want, nil
	} else if !errors.Is(statErr, ErrNotFound) && !errors.Is(statErr, fs.ErrNotExist) {
		return pluginapi.BlobRef{}, fmt.Errorf("check remote blob %s: %w", sum, statErr)
	}

	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return pluginapi.BlobRef{}, err
	}
	if err := tmp.Close(); err != nil {
		tmpOpen = false
		return pluginapi.BlobRef{}, fmt.Errorf("close remote upload temp: %w", err)
	}
	tmpOpen = false
	file, err := os.Open(tmpName)
	if err != nil {
		return pluginapi.BlobRef{}, fmt.Errorf("reopen remote upload temp: %w", err)
	}
	putErr := c.remote.Put(ctx, sum, file, n)
	fileCloseErr := file.Close()
	if fileCloseErr != nil {
		return pluginapi.BlobRef{}, errors.Join(putErr, fmt.Errorf("close remote upload source: %w", fileCloseErr))
	}
	if putErr != nil {
		if ctx.Err() != nil {
			return pluginapi.BlobRef{}, ctx.Err()
		}
		if verifyErr := c.verifyRemote(ctx, want); verifyErr == nil {
			return want, nil
		} else {
			return pluginapi.BlobRef{}, fmt.Errorf("remote put %s failed (%w); committed object was not verified: %v", sum, putErr, verifyErr)
		}
	}
	stored, err := c.StatContext(ctx, sum)
	if err != nil {
		return pluginapi.BlobRef{}, fmt.Errorf("verify remote blob %s after put: %w", sum, err)
	}
	if stored != want {
		return pluginapi.BlobRef{}, fmt.Errorf("%w: remote put returned object %+v, want %+v", ErrIntegrity, stored, want)
	}
	return want, nil
}

// GetContext returns fully downloaded and verified remote bytes in a seekable
// temporary file. Closing the reader removes that file.
func (c *CAS) GetContext(ctx context.Context, sum string) (reader io.ReadCloser, retErr error) {
	if err := requireContext(ctx); err != nil {
		return nil, err
	}
	if !validHash(sum) {
		return nil, fmt.Errorf("bad hash %q", sum)
	}
	if c.remote == nil {
		f, err := os.Open(c.path(sum))
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		return &contextFile{File: f, ctx: ctx}, nil
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	source, err := c.remote.Get(ctx, sum)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if source == nil {
		return nil, errors.New("remote backend returned a nil reader")
	}
	tmp, err := os.CreateTemp(c.tempRoot, "download-*.tmp")
	if err != nil {
		return nil, errors.Join(fmt.Errorf("create remote download temp: %w", err), source.Close())
	}
	removeOnFailure := true
	defer func() {
		if removeOnFailure {
			retErr = errors.Join(retErr, tmp.Close(), removeTemp(tmp.Name()))
		}
	}()

	h := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(tmp, h), contextReader{ctx: ctx, reader: source})
	sourceCloseErr := source.Close()
	if copyErr != nil {
		return nil, errors.Join(fmt.Errorf("download remote blob %s: %w", sum, copyErr), sourceCloseErr)
	}
	if sourceCloseErr != nil {
		return nil, fmt.Errorf("close remote blob %s: %w", sum, sourceCloseErr)
	}
	actual := hex.EncodeToString(h.Sum(nil))
	if actual != sum {
		return nil, fmt.Errorf("%w: remote blob %s contains digest %s", ErrIntegrity, sum, actual)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	removeOnFailure = false
	return &tempReadSeekCloser{File: tmp, path: tmp.Name(), ctx: ctx}, nil
}

// StatContext returns local file metadata or the remote backend's verified
// object metadata for a valid CAS key.
func (c *CAS) StatContext(ctx context.Context, sum string) (pluginapi.BlobRef, error) {
	if err := requireContext(ctx); err != nil {
		return pluginapi.BlobRef{}, err
	}
	if !validHash(sum) {
		return pluginapi.BlobRef{}, fmt.Errorf("bad hash %q", sum)
	}
	if c.remote != nil {
		object, err := c.remote.Stat(ctx, sum)
		if errors.Is(err, fs.ErrNotExist) {
			return pluginapi.BlobRef{}, ErrNotFound
		}
		if err != nil {
			return pluginapi.BlobRef{}, err
		}
		if err := validateRemoteObject(sum, object); err != nil {
			return pluginapi.BlobRef{}, err
		}
		return object.Ref, nil
	}
	fi, err := os.Stat(c.path(sum))
	if errors.Is(err, fs.ErrNotExist) {
		return pluginapi.BlobRef{}, ErrNotFound
	}
	if err != nil {
		return pluginapi.BlobRef{}, err
	}
	return pluginapi.BlobRef{SHA256: sum, Size: fi.Size()}, nil
}

// DeleteContext removes a blob. Remote backends are scoped to their owned
// namespace; missing objects remain an idempotent success.
func (c *CAS) DeleteContext(ctx context.Context, sum string) error {
	if err := requireContext(ctx); err != nil {
		return err
	}
	if !validHash(sum) {
		return fmt.Errorf("bad hash %q", sum)
	}
	if c.remote != nil {
		err := c.remote.Delete(ctx, sum)
		if errors.Is(err, ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	err := os.Remove(c.path(sum))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// ListContext reports every canonical local blob or every object exposed by
// the remote backend. RemoteObject.Modified is the provider mtime used by GC.
func (c *CAS) ListContext(ctx context.Context, fn func(RemoteObject) error) error {
	if err := requireContext(ctx); err != nil {
		return err
	}
	if fn == nil {
		return errors.New("blob list callback is required")
	}
	if c.remote != nil {
		return c.remote.List(ctx, func(object RemoteObject) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := validateRemoteObject(object.Ref.SHA256, object); err != nil {
				return err
			}
			return fn(object)
		})
	}
	return filepath.WalkDir(c.root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || !validHash(entry.Name()) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		return fn(RemoteObject{
			Ref:      pluginapi.BlobRef{SHA256: entry.Name(), Size: info.Size()},
			Modified: info.ModTime(),
		})
	})
}

// List walks every stored blob. Order is filesystem/provider-defined.
func (c *CAS) List(fn func(pluginapi.BlobRef) error) error {
	if fn == nil {
		return errors.New("blob list callback is required")
	}
	return c.ListContext(context.Background(), func(object RemoteObject) error {
		return fn(object.Ref)
	})
}

// Delete removes a blob and is idempotent when the key is already absent.
func (c *CAS) Delete(sum string) error {
	return c.DeleteContext(context.Background(), sum)
}

// MaterializeContext ensures verified bytes exist at the canonical local CAS
// path. Local CAS paths retain their existing behavior; remote materialization
// never replaces an existing file that fails integrity verification.
func (c *CAS) MaterializeContext(ctx context.Context, sum string) (result string, retErr error) {
	if err := requireContext(ctx); err != nil {
		return "", err
	}
	path, err := c.Path(sum)
	if err != nil {
		return "", err
	}
	if c.remote == nil {
		return path, nil
	}
	if err := ensureMaterializationDirectories(c.root, sum); err != nil {
		return "", err
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%w: materialized path is not a regular file: %s", ErrIntegrity, path)
		}
		actual, err := hashFile(ctx, path)
		if err != nil {
			return "", err
		}
		if actual != sum {
			return "", fmt.Errorf("%w: local materialization %s contains digest %s", ErrIntegrity, sum, actual)
		}
		return path, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}

	rc, err := c.GetContext(ctx, sum)
	if err != nil {
		return "", err
	}
	defer func() {
		retErr = errors.Join(retErr, rc.Close())
	}()
	tmp, err := os.CreateTemp(filepath.Dir(path), ".materialize-*.tmp")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	tmpOpen := true
	defer func() {
		if tmpOpen {
			retErr = errors.Join(retErr, tmp.Close())
		}
		retErr = errors.Join(retErr, removeTemp(tmpName))
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), contextReader{ctx: ctx, reader: rc})
	if err != nil {
		return "", err
	}
	if hex.EncodeToString(h.Sum(nil)) != sum {
		return "", fmt.Errorf("%w: materialized bytes changed while copying %s", ErrIntegrity, sum)
	}
	if err := tmp.Sync(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		tmpOpen = false
		return "", err
	}
	tmpOpen = false
	if err := installBlob(tmpName, path, n); err != nil {
		return "", fmt.Errorf("install materialized blob: %w", err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		return "", err
	}
	actual, err := hashFile(ctx, path)
	if err != nil {
		return "", err
	}
	if actual != sum {
		return "", fmt.Errorf("%w: installed materialization %s contains digest %s", ErrIntegrity, sum, actual)
	}
	return path, nil
}

func ensureMaterializationDirectories(root, sum string) error {
	dataDir := filepath.Dir(filepath.Dir(root))
	directories := []string{
		filepath.Join(dataDir, "blobs"),
		root,
		filepath.Join(root, sum[:2]),
		filepath.Join(root, sum[:2], sum[2:4]),
		filepath.Join(root, sum[:2], sum[2:4], sum[4:6]),
	}
	for _, path := range directories {
		if err := os.Mkdir(path, 0o750); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("create materialized shard directory %s: %w", path, err)
		}
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("inspect materialized shard directory %s: %w", path, err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("materialized shard path is not a real directory: %s", path)
		}
	}
	return nil
}

func (c *CAS) verifyRemote(ctx context.Context, want pluginapi.BlobRef) error {
	got, err := c.StatContext(ctx, want.SHA256)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%w: remote object is %+v, want %+v", ErrIntegrity, got, want)
	}
	rc, err := c.GetContext(ctx, want.SHA256)
	if err != nil {
		return err
	}
	return rc.Close()
}

func validateRemoteObject(sum string, object RemoteObject) error {
	if !validHash(sum) || object.Ref.SHA256 != sum || object.Ref.Size < 0 {
		return fmt.Errorf("%w: invalid remote object metadata for %q", ErrIntegrity, sum)
	}
	return nil
}

func requireContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("context is required")
	}
	return ctx.Err()
}

func removeTemp(path string) error {
	if path == "" {
		return nil
	}
	err := os.Remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

type contextFile struct {
	*os.File
	ctx context.Context
}

func (f *contextFile) Read(p []byte) (int, error) {
	if err := f.ctx.Err(); err != nil {
		return 0, err
	}
	return f.File.Read(p)
}

func (f *contextFile) ReadAt(p []byte, offset int64) (int, error) {
	if err := f.ctx.Err(); err != nil {
		return 0, err
	}
	return f.File.ReadAt(p, offset)
}

func (f *contextFile) WriteTo(w io.Writer) (int64, error) {
	return io.Copy(w, contextReader{ctx: f.ctx, reader: f.File})
}

type tempReadSeekCloser struct {
	*os.File
	path string
	ctx  context.Context
	once sync.Once
	err  error
}

func (f *tempReadSeekCloser) Read(p []byte) (int, error) {
	if err := f.ctx.Err(); err != nil {
		return 0, err
	}
	return f.File.Read(p)
}

func (f *tempReadSeekCloser) ReadAt(p []byte, offset int64) (int, error) {
	if err := f.ctx.Err(); err != nil {
		return 0, err
	}
	return f.File.ReadAt(p, offset)
}

func (f *tempReadSeekCloser) WriteTo(w io.Writer) (int64, error) {
	return io.Copy(w, contextReader{ctx: f.ctx, reader: f.File})
}

func (f *tempReadSeekCloser) Close() error {
	f.once.Do(func() {
		f.err = errors.Join(f.File.Close(), removeTemp(f.path))
	})
	return f.err
}
