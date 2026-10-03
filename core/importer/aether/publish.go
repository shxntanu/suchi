// SPDX-License-Identifier: AGPL-3.0-or-later

package aether

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/importer/bundle"
	"github.com/johnnybravo-xyz/suchi/core/slug"
)

func writeStagingBundle(stageDir string, documents []*sourceEntry, report *Report) error {
	nameSet := make(map[string]struct{})
	for _, document := range documents {
		for _, tag := range document.Tags {
			nameSet[tag.DisplayName] = struct{}{}
		}
	}
	tagNames := make([]string, 0, len(nameSet))
	for name := range nameSet {
		tagNames = append(tagNames, name)
	}
	sort.Strings(tagNames)

	tagIDs := make(map[string]int64, len(tagNames))
	usedSlugs := make(map[string]struct{}, len(tagNames))
	manifest := make(bundle.Manifest, 0, len(tagNames)+len(documents))
	for index, name := range tagNames {
		pk := int64(index + 1)
		tagIDs[name] = pk
		tagSlug := slug.Make(name)
		if tagSlug == "" {
			tagSlug = "tag"
		}
		base := tagSlug
		for suffix := 2; ; suffix++ {
			if _, exists := usedSlugs[tagSlug]; !exists {
				usedSlugs[tagSlug] = struct{}{}
				break
			}
			tagSlug = fmt.Sprintf("%s-%d", base, suffix)
		}
		fields, err := json.Marshal(bundle.TagFields{Name: name, Slug: tagSlug})
		if err != nil {
			return fmt.Errorf("encode tag %q: %w", name, err)
		}
		manifest = append(manifest, bundle.Object{Model: bundle.ModelTag, PK: pk, Fields: fields})
	}

	for _, entry := range documents {
		if entry.Document == nil {
			return errors.New("validated document has no manifest")
		}
		tagPKs := make([]int64, 0, len(entry.Tags))
		for _, tag := range entry.Tags {
			pk, ok := tagIDs[tag.DisplayName]
			if !ok {
				return fmt.Errorf("validated tag %q has no bundle identity", tag.DisplayName)
			}
			tagPKs = append(tagPKs, pk)
		}
		created := entry.Document.CreatedAt.UTC().Format(time.RFC3339Nano)
		fields := bundle.DocumentFields{
			Title:            entry.Document.Title,
			MimeType:         entry.Document.MediaType,
			Checksum:         entry.MD5,
			OriginalFilename: entry.Document.OriginalFilename,
			OriginalPath:     entry.UUID,
			Created:          created,
			Modified:         entry.Document.UpdatedAt.UTC().Format(time.RFC3339Nano),
			Added:            created,
			Tags:             tagPKs,
		}
		encoded, err := json.Marshal(fields)
		if err != nil {
			return fmt.Errorf("encode document %s: %w", entry.UUID, err)
		}
		manifest = append(manifest, bundle.Object{Model: bundle.ModelDocument, PK: entry.LegacyID, Fields: encoded})
	}

	encodedManifest, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode bundle manifest: %w", err)
	}
	if err := writeExclusive(filepath.Join(stageDir, "manifest.json"), encodedManifest); err != nil {
		return fmt.Errorf("write bundle manifest: %w", err)
	}

	reportDir := filepath.Join(stageDir, "aether-migration")
	if err := os.Mkdir(reportDir, 0o700); err != nil {
		return fmt.Errorf("create migration report directory: %w", err)
	}
	encodedReport, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encode migration report: %w", err)
	}
	if err := writeExclusive(filepath.Join(reportDir, "report.json"), encodedReport); err != nil {
		return fmt.Errorf("write migration report: %w", err)
	}
	return nil
}

func writeExclusive(path string, body []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	n, writeErr := file.Write(body)
	if writeErr == nil && n != len(body) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil {
		_ = os.Remove(path)
		return writeErr
	}
	if closeErr != nil {
		_ = os.Remove(path)
		return closeErr
	}
	return nil
}

// publishStagingDirectory creates the requested destination exclusively, then
// copies the already-verified bundle into it using create-exclusive files.
// Existing paths are never replaced. If publication fails, it removes only
// paths created by this call and leaves unrelated operator files untouched.
func publishStagingDirectory(ctx context.Context, stageDir, outputDir string) error {
	if err := os.Mkdir(outputDir, 0o700); err != nil {
		return fmt.Errorf("create output directory exclusively: %w", err)
	}
	created := []string{outputDir}
	rollback := func() {
		for index := len(created) - 1; index >= 0; index-- {
			_ = os.Remove(created[index])
		}
	}
	err := filepath.WalkDir(stageDir, func(sourcePath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, err := filepath.Rel(stageDir, sourcePath)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		destinationPath := filepath.Join(outputDir, relative)
		if entry.IsDir() {
			if err := os.Mkdir(destinationPath, 0o700); err != nil {
				return err
			}
			created = append(created, destinationPath)
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("staging bundle contains a non-regular file")
		}
		if err := copyFileExclusive(ctx, sourcePath, destinationPath, info.Mode().Perm()); err != nil {
			return err
		}
		created = append(created, destinationPath)
		return nil
	})
	if err != nil {
		rollback()
		return err
	}
	return nil
}

func copyFileExclusive(ctx context.Context, sourcePath, destinationPath string, mode os.FileMode) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()
	destination, err := os.OpenFile(destinationPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	copyErr := copyContext(ctx, destination, source)
	if copyErr == nil {
		copyErr = destination.Sync()
	}
	closeErr := destination.Close()
	if copyErr != nil {
		_ = os.Remove(destinationPath)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(destinationPath)
		return closeErr
	}
	return nil
}

func copyContext(ctx context.Context, destination io.Writer, source io.Reader) error {
	buf := make([]byte, 128*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := source.Read(buf)
		if n > 0 {
			written, writeErr := destination.Write(buf[:n])
			if writeErr != nil {
				return writeErr
			}
			if written != n {
				return io.ErrShortWrite
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
}
