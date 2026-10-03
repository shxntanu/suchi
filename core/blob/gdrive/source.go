// SPDX-License-Identifier: AGPL-3.0-or-later

package gdrive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
)

// Source provides read-only access to files marked with Aether storage keys
// directly beneath a configured Drive parent folder.
type Source struct {
	drive    *driveClient
	parentID string

	mu       sync.RWMutex
	listedID map[string]string
}

// NewSource validates the parent folder but never creates or changes Drive
// files or folders.
func NewSource(ctx context.Context, config Config) (*Source, error) {
	drive, err := newDriveClient(config)
	if err != nil {
		return nil, err
	}
	if err := drive.validateParent(ctx, config.FolderID); err != nil {
		return nil, err
	}
	return &Source{
		drive:    drive,
		parentID: config.FolderID,
		listedID: make(map[string]string),
	}, nil
}

// List returns every Aether storage key and its file ID from the configured
// parent, following Drive's page tokens and rejecting duplicate keys.
func (s *Source) List(ctx context.Context) (map[string]string, error) {
	q := fmt.Sprintf("'%s' in parents and appProperties has { key='%s' } and trashed = false",
		escapeDriveQuery(s.parentID), aetherStorageKey)
	files, err := s.drive.listFiles(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list Aether files from Google Drive: %w", err)
	}
	byKey := make(map[string]string, len(files))
	byID := make(map[string]string, len(files))
	for _, file := range files {
		key := file.AppProperties[aetherStorageKey]
		if file.ID == "" || key == "" || file.Trashed || !hasParent(file, s.parentID) {
			return nil, errors.New("Aether Drive file has a missing storage key, missing ID, or is outside the configured parent")
		}
		if _, exists := byKey[key]; exists {
			return nil, fmt.Errorf("duplicate Aether Drive storage key %q", key)
		}
		if _, exists := byID[file.ID]; exists {
			return nil, errors.New("duplicate Aether Drive file ID in parent inventory")
		}
		byKey[key] = file.ID
		byID[file.ID] = key
	}

	s.mu.Lock()
	s.listedID = byID
	s.mu.Unlock()
	return byKey, nil
}

// Open opens a file ID returned by the most recent successful List call.
// The file's current parent and storage-key marker are checked again before
// its bytes are read.
func (s *Source) Open(ctx context.Context, id string) (io.ReadCloser, error) {
	s.mu.RLock()
	key, listed := s.listedID[id]
	s.mu.RUnlock()
	if !listed || id == "" {
		return nil, errors.New("Aether Drive file ID was not returned by List")
	}

	query := url.Values{
		"fields":            []string{"id,parents,appProperties,trashed"},
		"supportsAllDrives": []string{"true"},
	}
	var file driveFile
	if err := s.drive.getJSON(ctx, s.drive.apiFileURL(id, query), &file); err != nil {
		return nil, fmt.Errorf("verify listed Aether Drive file: %w", err)
	}
	if file.ID != id || file.Trashed || !hasParent(file, s.parentID) || file.AppProperties[aetherStorageKey] != key {
		return nil, errors.New("listed Aether Drive file is no longer in the configured parent")
	}

	mediaQuery := url.Values{
		"alt":               []string{"media"},
		"supportsAllDrives": []string{"true"},
	}
	resp, err := s.drive.request(ctx, http.MethodGet, s.drive.apiFileURL(id, mediaQuery), nil, make(http.Header))
	if err != nil {
		return nil, fmt.Errorf("download listed Aether Drive file: %w", err)
	}
	return resp.Body, nil
}
