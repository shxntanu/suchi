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
	"sort"
)

// ScrubOptions controls the optional repair action. The zero value only reports.
type ScrubOptions struct {
	Quarantine bool
}

// ScrubReport describes one complete CAS integrity pass.
type ScrubReport struct {
	Referenced   int
	Checked      int
	BytesChecked int64
	Missing      []string
	Corrupt      []CorruptBlob
	Failures     []ScrubFailure
}

// CorruptBlob records a file whose content does not match its CAS key.
type CorruptBlob struct {
	Expected      string
	Actual        string
	Size          int64
	QuarantinedTo string
}

// ScrubFailure records a path that could not be inspected or quarantined.
type ScrubFailure struct {
	Path string
	Err  string
}

// Scrub hashes every canonical CAS file and checks that each expected hash exists.
// Non-hash expected values are ignored because demo metadata can use sentinel keys.
func (c *CAS) Scrub(ctx context.Context, expected []string, opts ScrubOptions) (*ScrubReport, error) {
	report := &ScrubReport{}
	if c.remote != nil && opts.Quarantine {
		return report, ErrRemoteQuarantineUnsupported
	}
	if err := requireContext(ctx); err != nil {
		return report, err
	}
	if c.remote != nil {
		return c.scrubRemote(ctx, expected, report)
	}
	present := make(map[string]bool)

	err := filepath.WalkDir(c.root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !validHash(entry.Name()) {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		expectedHash := entry.Name()
		if filepath.Clean(path) != c.path(expectedHash) {
			report.Failures = append(report.Failures, ScrubFailure{
				Path: path,
				Err:  "hash file is outside its canonical shard",
			})
			return nil
		}
		present[expectedHash] = true

		info, err := entry.Info()
		if err != nil {
			report.Failures = append(report.Failures, ScrubFailure{Path: path, Err: err.Error()})
			return nil
		}
		if !info.Mode().IsRegular() {
			report.Failures = append(report.Failures, ScrubFailure{Path: path, Err: "not a regular file"})
			return nil
		}
		actualHash, err := hashFile(ctx, path)
		if err != nil {
			report.Failures = append(report.Failures, ScrubFailure{Path: path, Err: err.Error()})
			return nil
		}
		report.Checked++
		report.BytesChecked += info.Size()
		if actualHash == expectedHash {
			return nil
		}

		corrupt := CorruptBlob{Expected: expectedHash, Actual: actualHash, Size: info.Size()}
		if opts.Quarantine {
			quarantinedTo, err := c.quarantine(path, expectedHash, actualHash)
			if err != nil {
				report.Failures = append(report.Failures, ScrubFailure{Path: path, Err: "quarantine: " + err.Error()})
			} else {
				corrupt.QuarantinedTo = quarantinedTo
			}
		}
		report.Corrupt = append(report.Corrupt, corrupt)
		return nil
	})
	if err != nil {
		return report, fmt.Errorf("walk CAS: %w", err)
	}

	seenExpected := make(map[string]bool)
	for _, sum := range expected {
		if !validHash(sum) || seenExpected[sum] {
			continue
		}
		seenExpected[sum] = true
		report.Referenced++
		if !present[sum] {
			report.Missing = append(report.Missing, sum)
		}
	}
	sort.Strings(report.Missing)
	sort.Slice(report.Corrupt, func(i, j int) bool {
		return report.Corrupt[i].Expected < report.Corrupt[j].Expected
	})
	sort.Slice(report.Failures, func(i, j int) bool {
		return report.Failures[i].Path < report.Failures[j].Path
	})
	return report, nil
}

func (c *CAS) scrubRemote(ctx context.Context, expected []string, report *ScrubReport) (*ScrubReport, error) {
	present := make(map[string]bool)
	seen := make(map[string]bool)
	err := c.remote.List(ctx, func(object RemoteObject) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		sum := object.Ref.SHA256
		if !validHash(sum) || object.Ref.Size < 0 {
			report.Failures = append(report.Failures, ScrubFailure{
				Path: "remote:" + sum,
				Err:  "invalid remote object metadata",
			})
			return nil
		}
		present[sum] = true
		if seen[sum] {
			report.Failures = append(report.Failures, ScrubFailure{
				Path: "remote:" + sum,
				Err:  "duplicate remote object for one CAS key",
			})
			return nil
		}
		seen[sum] = true

		source, err := c.remote.Get(ctx, sum)
		if err != nil {
			if errors.Is(err, ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
				report.Failures = append(report.Failures, ScrubFailure{Path: "remote:" + sum, Err: ErrNotFound.Error()})
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			report.Failures = append(report.Failures, ScrubFailure{Path: "remote:" + sum, Err: err.Error()})
			return nil
		}
		if source == nil {
			report.Failures = append(report.Failures, ScrubFailure{Path: "remote:" + sum, Err: "remote backend returned a nil reader"})
			return nil
		}
		h := sha256.New()
		n, readErr := io.Copy(h, contextReader{ctx: ctx, reader: source})
		closeErr := source.Close()
		if readErr != nil || closeErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			failure := errors.Join(readErr, closeErr)
			report.Failures = append(report.Failures, ScrubFailure{Path: "remote:" + sum, Err: failure.Error()})
			return nil
		}
		report.Checked++
		report.BytesChecked += n
		actual := hex.EncodeToString(h.Sum(nil))
		if actual != sum {
			report.Corrupt = append(report.Corrupt, CorruptBlob{Expected: sum, Actual: actual, Size: n})
		}
		if n != object.Ref.Size {
			report.Failures = append(report.Failures, ScrubFailure{
				Path: "remote:" + sum,
				Err:  fmt.Sprintf("remote metadata size %d differs from downloaded size %d", object.Ref.Size, n),
			})
		}
		return nil
	})
	if err != nil {
		return report, fmt.Errorf("list remote CAS: %w", err)
	}

	seenExpected := make(map[string]bool)
	for _, sum := range expected {
		if !validHash(sum) || seenExpected[sum] {
			continue
		}
		seenExpected[sum] = true
		report.Referenced++
		if !present[sum] {
			report.Missing = append(report.Missing, sum)
		}
	}
	sort.Strings(report.Missing)
	sort.Slice(report.Corrupt, func(i, j int) bool {
		return report.Corrupt[i].Expected < report.Corrupt[j].Expected
	})
	sort.Slice(report.Failures, func(i, j int) bool {
		return report.Failures[i].Path < report.Failures[j].Path
	})
	return report, nil
}

func hashFile(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, contextReader{ctx: ctx, reader: f}); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (c *CAS) quarantine(path, expected, actual string) (string, error) {
	dir := filepath.Join(filepath.Dir(c.root), "quarantine")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	base := filepath.Join(dir, expected+"."+actual)
	destination := base
	for suffix := 1; ; suffix++ {
		_, err := os.Lstat(destination)
		if errors.Is(err, fs.ErrNotExist) {
			break
		}
		if err != nil {
			return "", err
		}
		destination = fmt.Sprintf("%s.%d", base, suffix)
	}
	if err := os.Rename(path, destination); err != nil {
		return "", err
	}
	return destination, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
