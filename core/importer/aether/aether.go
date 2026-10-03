// SPDX-License-Identifier: AGPL-3.0-or-later

// Package aether converts read-only Aether Drive objects into Suchi's
// Paperless-compatible import bundle.
package aether

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	manifestSuffix  = ".manifest.json"
	maxManifestSize = 4 << 20
)

// Source is the read-only Aether object inventory used by the converter.
// List maps Aether storage keys to provider file IDs; Open reads one object
// without granting the converter any mutation capability.
type Source interface {
	List(context.Context) (map[string]string, error)
	Open(context.Context, string) (io.ReadCloser, error)
}

// Options selects the explicit bundle destination and whether to only verify
// the source and report its projected Suchi storage size.
type Options struct {
	OutputDir string
	DryRun    bool
}

// Report summarizes the records considered by a conversion. Tags counts the
// distinct display names included in the validated ready documents. Documents
// counts ready documents whose manifest and original both passed validation.
// Complete is false whenever a source or integrity failure prevented a full
// inventory; skipped non-ready states are complete inventory entries.
type Report struct {
	Documents      int             `json:"documents"`
	Tags           int             `json:"tags"`
	Skipped        int             `json:"skipped"`
	Failures       int             `json:"failures"`
	ProjectedBytes int64           `json:"projected_bytes"`
	Complete       bool            `json:"complete"`
	Mappings       []MappingRecord `json:"mappings"`
	Diagnostics    []Diagnostic    `json:"diagnostics"`
}

// MappingRecord preserves the source UUID and the stable legacy integer used
// by Suchi's existing bundle importer. SHA256 identifies the verified source
// bytes without exposing a Drive object ID.
type MappingRecord struct {
	SourceUUID string `json:"source_uuid"`
	LegacyID   int64  `json:"legacy_id"`
	SHA256     string `json:"sha256"`
}

// Diagnostic records one source inventory or validation problem. It contains
// no provider response bodies, file IDs, or credentials.
type Diagnostic struct {
	StorageKey string `json:"storage_key,omitempty"`
	SourceUUID string `json:"source_uuid,omitempty"`
	Reason     string `json:"reason"`
}

type sourceManifest struct {
	SchemaVersion int            `json:"schemaVersion"`
	Document      sourceDocument `json:"document"`
	Tags          []sourceTag    `json:"tags"`
}

type sourceDocument struct {
	ID               string    `json:"id"`
	Title            string    `json:"title"`
	OriginalFilename string    `json:"originalFilename"`
	MediaType        string    `json:"mediaType"`
	SizeBytes        int64     `json:"sizeBytes"`
	SHA256           string    `json:"sha256"`
	Status           string    `json:"status"`
	CreatedAt        time.Time `json:"createdAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
}

type sourceTag struct {
	ID             string `json:"id"`
	DisplayName    string `json:"displayName"`
	NormalizedName string `json:"normalizedName"`
	Implicit       bool   `json:"implicit"`
}

type sourceEntry struct {
	UUID          string
	StorageUUID   string
	OriginalID    string
	ManifestID    string
	ManifestKey   string
	OriginalKey   string
	InvalidReason string
	Document      *sourceDocument
	Tags          []sourceTag
	LegacyID      int64
	SHA256        string
	MD5           string
}

// Run reads and verifies every Aether document manifest, then calculates the
// number of unique original bytes Suchi would store. In dry-run mode it makes
// no filesystem changes. Bundle publication is kept separate from inventory
// validation so a corrupt source can never produce a misleading partial
// import bundle.
func Run(ctx context.Context, src Source, opts Options) (*Report, error) {
	report := &Report{
		Mappings:    []MappingRecord{},
		Diagnostics: []Diagnostic{},
	}
	if src == nil {
		return report, errors.New("aether migration: source is required")
	}
	if opts.OutputDir == "" {
		return report, errors.New("aether migration: explicit OutputDir is required")
	}
	outputDir, err := filepath.Abs(opts.OutputDir)
	if err != nil {
		return report, fmt.Errorf("aether migration: resolve output path: %w", err)
	}
	if err := ensureOutputAvailable(outputDir); err != nil {
		return report, err
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}

	listed, err := src.List(ctx)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return report, ctxErr
		}
		addFailure(report, nil, "source inventory could not be listed")
		return report, errors.New("aether migration: source inventory could not be listed")
	}
	entries := inventoryEntries(listed, report)
	valid := make([]*sourceEntry, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if entry.InvalidReason != "" {
			addFailure(report, entry, entry.InvalidReason)
			continue
		}
		if entry.ManifestID == "" {
			addFailure(report, entry, "manifest is missing")
			continue
		}
		manifestBytes, err := readSourceObject(ctx, src, entry.ManifestID, maxManifestSize)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return report, ctxErr
			}
			addFailure(report, entry, "manifest could not be read")
			continue
		}
		manifest, err := parseManifest(manifestBytes)
		if err != nil {
			addFailure(report, entry, "manifest schema is invalid")
			continue
		}
		docUUID, _, err := parseUUID(manifest.Document.ID)
		if err != nil {
			addFailure(report, entry, "document UUID is invalid")
			continue
		}
		if docUUID != entry.UUID {
			addFailure(report, entry, "document UUID does not match its storage key")
			continue
		}
		entry.Document = &manifest.Document
		entry.Tags = manifest.Tags
		switch manifest.Document.Status {
		case "uploading", "failed", "deleted":
			report.Skipped++
			continue
		case "ready":
		default:
			addFailure(report, entry, "document status is invalid")
			continue
		}
		if err := validateReadyDocument(manifest.Document); err != nil {
			addFailure(report, entry, err.Error())
			continue
		}
		if err := validateTags(manifest.Tags); err != nil {
			addFailure(report, entry, err.Error())
			continue
		}
		valid = append(valid, entry)
	}

	ids, collisionGroups, idErr := buildLegacyIDs(valid)
	if idErr != nil {
		for _, entry := range valid {
			addFailure(report, entry, "document UUID cannot be mapped to a legacy ID")
		}
		report.Complete = false
		return report, fmt.Errorf("aether migration: derive legacy IDs: %w", idErr)
	}
	collided := map[string]bool{}
	for _, group := range collisionGroups {
		for _, uuid := range group {
			collided[uuid] = true
			addFailure(report, entryByUUID(valid, uuid), "deterministic legacy integer ID collision")
		}
	}

	stageDir := ""
	if !opts.DryRun {
		stageDir, err = os.MkdirTemp(filepath.Dir(outputDir), ".suchi-aether-migration-")
		if err != nil {
			addFailure(report, nil, "temporary bundle staging could not be created")
			return report, fmt.Errorf("aether migration: create staging directory: %w", err)
		}
		defer os.RemoveAll(stageDir)
		if err := os.Mkdir(filepath.Join(stageDir, "originals"), 0o700); err != nil {
			addFailure(report, nil, "temporary bundle staging could not be created")
			return report, fmt.Errorf("aether migration: create originals staging directory: %w", err)
		}
	}

	projected := make(map[string]int64)
	tagNames := make(map[string]struct{})
	verified := make([]*sourceEntry, 0, len(valid))
	for _, entry := range valid {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if collided[entry.UUID] {
			continue
		}
		entry.LegacyID = ids[entry.UUID]
		if entry.OriginalID == "" {
			addFailure(report, entry, "original is missing")
			continue
		}

		var stagedFile *os.File
		var destination io.Writer = io.Discard
		if stageDir != "" {
			path := filepath.Join(stageDir, "originals", entry.UUID)
			stagedFile, err = os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				addFailure(report, entry, "original could not be staged")
				continue
			}
			destination = stagedFile
		}
		sha, md5sum, size, err := verifyOriginal(ctx, src, entry.OriginalID, entry.Document.SizeBytes, entry.Document.SHA256, destination)
		if stagedFile != nil {
			if syncErr := stagedFile.Sync(); err == nil && syncErr != nil {
				err = syncErr
			}
			if closeErr := stagedFile.Close(); err == nil && closeErr != nil {
				err = closeErr
			}
			if err != nil {
				_ = os.Remove(filepath.Join(stageDir, "originals", entry.UUID))
			}
		}
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return report, ctxErr
			}
			addFailure(report, entry, integrityReason(err))
			continue
		}
		entry.SHA256 = sha
		entry.MD5 = md5sum
		if oldSize, ok := projected[sha]; ok {
			if oldSize != size {
				addFailure(report, entry, "identical SHA-256 has conflicting sizes")
				continue
			}
		} else {
			if size > math.MaxInt64-report.ProjectedBytes {
				addFailure(report, entry, "projected byte count overflows")
				continue
			}
			projected[sha] = size
			report.ProjectedBytes += size
		}
		for _, tag := range entry.Tags {
			tagNames[tag.DisplayName] = struct{}{}
		}
		report.Documents++
		report.Mappings = append(report.Mappings, MappingRecord{
			SourceUUID: entry.UUID,
			LegacyID:   entry.LegacyID,
			SHA256:     sha,
		})
		verified = append(verified, entry)
	}
	report.Tags = len(tagNames)
	sort.Slice(report.Mappings, func(i, j int) bool {
		return report.Mappings[i].SourceUUID < report.Mappings[j].SourceUUID
	})
	report.Complete = report.Failures == 0
	if report.Failures > 0 {
		return report, fmt.Errorf("aether migration: %d source failure(s); no bundle was published", report.Failures)
	}
	if opts.DryRun {
		return report, nil
	}
	if err := writeStagingBundle(stageDir, verified, report); err != nil {
		addFailure(report, nil, "bundle files could not be written")
		report.Complete = false
		return report, fmt.Errorf("aether migration: write bundle staging: %w", err)
	}
	if err := publishStagingDirectory(ctx, stageDir, outputDir); err != nil {
		addFailure(report, nil, "bundle output could not be published")
		report.Complete = false
		return report, fmt.Errorf("aether migration: publish bundle: %w", err)
	}
	return report, nil
}

func ensureOutputAvailable(outputDir string) error {
	if _, err := os.Lstat(outputDir); err == nil {
		return fmt.Errorf("aether migration: output path already exists: %s", outputDir)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("aether migration: inspect output path: %w", err)
	}
	parent := filepath.Dir(outputDir)
	info, err := os.Stat(parent)
	if err != nil {
		return fmt.Errorf("aether migration: output parent is unavailable: %w", err)
	}
	if !info.IsDir() {
		return errors.New("aether migration: output parent is not a directory")
	}
	return nil
}

func inventoryEntries(objects map[string]string, report *Report) []*sourceEntry {
	byUUID := make(map[string]*sourceEntry)
	keys := make([]string, 0, len(objects))
	for key := range objects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	seenStorageIDs := make(map[string]string)
	for _, key := range keys {
		if !strings.HasPrefix(key, "documents/") {
			continue
		}
		storageUUID, kind, err := parseStorageKey(key)
		if err != nil {
			addFailure(report, &sourceEntry{ManifestKey: key, OriginalKey: key}, "document storage key is invalid")
			continue
		}
		if storageUUID == "" {
			addFailure(report, &sourceEntry{ManifestKey: key, OriginalKey: key}, "document storage key is invalid")
			continue
		}
		uuid, _, _ := parseUUID(storageUUID)
		entry := byUUID[uuid]
		if entry == nil {
			entry = &sourceEntry{UUID: uuid, StorageUUID: storageUUID}
			byUUID[uuid] = entry
		}
		if prior, ok := seenStorageIDs[uuid]; ok && prior != storageUUID {
			entry.InvalidReason = "document storage key UUID is duplicated with different casing"
		} else {
			seenStorageIDs[uuid] = storageUUID
		}
		fileID := objects[key]
		if fileID == "" {
			entry.InvalidReason = "source object ID is empty"
		}
		switch kind {
		case "original":
			if entry.OriginalKey != "" {
				entry.InvalidReason = "multiple originals use one document UUID"
			}
			entry.OriginalKey = key
			entry.OriginalID = fileID
		case "manifest":
			if entry.ManifestKey != "" {
				entry.InvalidReason = "multiple manifests use one document UUID"
			}
			entry.ManifestKey = key
			entry.ManifestID = fileID
		}
	}

	uuids := make([]string, 0, len(byUUID))
	for uuid := range byUUID {
		uuids = append(uuids, uuid)
	}
	sort.Strings(uuids)
	entries := make([]*sourceEntry, 0, len(uuids))
	for _, uuid := range uuids {
		entries = append(entries, byUUID[uuid])
	}
	return entries
}

func parseStorageKey(key string) (uuid, kind string, err error) {
	parts := strings.Split(key, "/")
	if len(parts) != 3 || parts[0] != "documents" {
		return "", "", errors.New("invalid Aether document key")
	}
	if _, _, err := parseUUID(parts[1]); err != nil {
		return "", "", err
	}
	switch parts[2] {
	case "original":
		return parts[1], "original", nil
	case "original" + manifestSuffix:
		return parts[1], "manifest", nil
	default:
		return "", "", errors.New("unknown Aether object suffix")
	}
}

func parseManifest(data []byte) (sourceManifest, error) {
	var manifest sourceManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return sourceManifest{}, err
	}
	if manifest.SchemaVersion != 1 {
		return sourceManifest{}, errors.New("unsupported manifest schema")
	}
	return manifest, nil
}

func validateReadyDocument(document sourceDocument) error {
	if document.Title == "" {
		return errors.New("ready document title is empty")
	}
	if !safeFilename(document.OriginalFilename) {
		return errors.New("ready document filename is unsafe")
	}
	if document.SizeBytes < 0 {
		return errors.New("ready document size is invalid")
	}
	if !validSHA256(document.SHA256) {
		return errors.New("ready document SHA-256 is invalid")
	}
	if document.CreatedAt.IsZero() || document.UpdatedAt.IsZero() {
		return errors.New("ready document timestamp is invalid")
	}
	return nil
}

func validateTags(tags []sourceTag) error {
	seen := make(map[string]struct{}, len(tags))
	for _, tag := range tags {
		if _, _, err := parseUUID(tag.ID); err != nil {
			return errors.New("tag UUID is invalid")
		}
		if strings.TrimSpace(tag.DisplayName) == "" || !utf8.ValidString(tag.DisplayName) {
			return errors.New("tag name is invalid")
		}
		normalized := strings.ToLower(strings.Join(strings.Fields(tag.DisplayName), " "))
		if tag.NormalizedName != normalized {
			return errors.New("tag normalized name is invalid")
		}
		if _, ok := seen[normalized]; ok {
			return errors.New("document contains duplicate tags")
		}
		seen[normalized] = struct{}{}
	}
	return nil
}

func safeFilename(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > 255 || !utf8.ValidString(name) {
		return false
	}
	if filepath.IsAbs(name) || filepath.Base(name) != name || strings.ContainsAny(name, `/\\`) {
		return false
	}
	if strings.TrimSpace(name) != name {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validSHA256(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func readSourceObject(ctx context.Context, src Source, fileID string, limit int) ([]byte, error) {
	reader, err := src.Open(ctx, fileID)
	if err != nil {
		return nil, err
	}
	if reader == nil {
		return nil, errors.New("source returned an empty reader")
	}
	data, readErr := readAllContext(ctx, reader, int64(limit))
	closeErr := reader.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return data, nil
}

func readAllContext(ctx context.Context, src io.Reader, limit int64) ([]byte, error) {
	var output bytes.Buffer
	buf := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := src.Read(buf)
		if n > 0 {
			if int64(output.Len())+int64(n) > limit {
				return nil, errors.New("source object exceeds size limit")
			}
			_, _ = output.Write(buf[:n])
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return output.Bytes(), nil
			}
			return nil, err
		}
		if n == 0 {
			return nil, io.ErrNoProgress
		}
	}
}

func verifyOriginal(ctx context.Context, src Source, fileID string, expectedSize int64, expectedSHA string, destination io.Writer) (sha256Hex, md5Hex string, size int64, err error) {
	reader, err := src.Open(ctx, fileID)
	if err != nil {
		return "", "", 0, err
	}
	if reader == nil {
		return "", "", 0, errors.New("source returned an empty reader")
	}
	if destination == nil {
		destination = io.Discard
	}
	sha, md5sum, size, readErr := hashCopyContext(ctx, destination, reader)
	closeErr := reader.Close()
	if readErr != nil {
		return "", "", size, readErr
	}
	if closeErr != nil {
		return "", "", size, closeErr
	}
	if size != expectedSize {
		return "", "", size, errOriginalSize
	}
	if sha != expectedSHA {
		return "", "", size, errOriginalHash
	}
	return sha, md5sum, size, nil
}

var errOriginalSize = errors.New("original size mismatch")
var errOriginalHash = errors.New("original sha256 mismatch")

func hashCopyContext(ctx context.Context, destination io.Writer, reader io.Reader) (sha256Hex, md5Hex string, size int64, err error) {
	sha := sha256.New()
	md5sum := md5.New()
	buf := make([]byte, 128*1024)
	for {
		if err := ctx.Err(); err != nil {
			return "", "", size, err
		}
		n, readErr := reader.Read(buf)
		if n > 0 {
			written, writeErr := destination.Write(buf[:n])
			if writeErr != nil {
				return "", "", size, writeErr
			}
			if written != n {
				return "", "", size, io.ErrShortWrite
			}
			if size > math.MaxInt64-int64(written) {
				return "", "", size, errors.New("original byte count overflows")
			}
			_, _ = sha.Write(buf[:written])
			_, _ = md5sum.Write(buf[:written])
			size += int64(written)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return hex.EncodeToString(sha.Sum(nil)), hex.EncodeToString(md5sum.Sum(nil)), size, nil
			}
			return "", "", size, readErr
		}
		if n == 0 {
			return "", "", size, io.ErrNoProgress
		}
	}
}

func integrityReason(err error) string {
	if errors.Is(err, errOriginalSize) {
		return "original size does not match manifest"
	}
	if errors.Is(err, errOriginalHash) {
		return "original SHA-256 does not match manifest"
	}
	return "original could not be read"
}

func addFailure(report *Report, entry *sourceEntry, reason string) {
	report.Failures++
	diagnostic := Diagnostic{Reason: reason}
	if entry != nil {
		diagnostic.StorageKey = entry.ManifestKey
		if diagnostic.StorageKey == "" {
			diagnostic.StorageKey = entry.OriginalKey
		}
		diagnostic.SourceUUID = entry.UUID
	}
	report.Diagnostics = append(report.Diagnostics, diagnostic)
}

func parseUUID(value string) (normalized string, bytes [16]byte, err error) {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return "", bytes, errors.New("UUID must use canonical hyphenated form")
	}
	compact := strings.ReplaceAll(value, "-", "")
	decoded, err := hex.DecodeString(compact)
	if err != nil || len(decoded) != len(bytes) {
		return "", bytes, errors.New("UUID contains invalid hexadecimal")
	}
	version := decoded[6] >> 4
	variant := decoded[8] >> 6
	if version < 1 || version > 8 || variant != 2 {
		return "", bytes, errors.New("UUID version or variant is invalid")
	}
	copy(bytes[:], decoded)
	normalized = strings.ToLower(value)
	return normalized, bytes, nil
}

func legacyIDForUUID(value string) (int64, error) {
	_, raw, err := parseUUID(value)
	if err != nil {
		return 0, err
	}
	legacyID := int64(binary.BigEndian.Uint64(raw[:8]) & math.MaxInt64)
	if legacyID <= 0 {
		return 0, errors.New("UUID maps to a non-positive legacy ID")
	}
	return legacyID, nil
}

func buildLegacyIDs(entries []*sourceEntry) (map[string]int64, [][]string, error) {
	ids := make(map[string]int64, len(entries))
	owners := make(map[int64][]string, len(entries))
	for _, entry := range entries {
		legacyID, err := legacyIDForUUID(entry.UUID)
		if err != nil {
			return nil, nil, err
		}
		ids[entry.UUID] = legacyID
		owners[legacyID] = append(owners[legacyID], entry.UUID)
	}
	collisions := make([][]string, 0)
	for _, uuids := range owners {
		if len(uuids) > 1 {
			sort.Strings(uuids)
			collisions = append(collisions, uuids)
			for _, uuid := range uuids {
				delete(ids, uuid)
			}
		}
	}
	sort.Slice(collisions, func(i, j int) bool { return collisions[i][0] < collisions[j][0] })
	return ids, collisions, nil
}

func entryByUUID(entries []*sourceEntry, uuid string) *sourceEntry {
	for _, entry := range entries {
		if entry.UUID == uuid {
			return entry
		}
	}
	return nil
}
