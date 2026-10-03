// SPDX-License-Identifier: AGPL-3.0-or-later

package gdrive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// Store is the Suchi blob backend scoped to one marked child folder.
type Store struct {
	drive       *driveClient
	namespaceID string
	putMu       sync.Mutex
}

// New validates the configured parent and resolves the marked Suchi namespace.
// It creates the namespace only when Config.CreateNamespace is true.
func New(ctx context.Context, config Config) (*Store, error) {
	drive, err := newDriveClient(config)
	if err != nil {
		return nil, err
	}
	if err := drive.validateParent(ctx, config.FolderID); err != nil {
		return nil, err
	}
	namespaceID, found, err := drive.findNamespace(ctx, config.FolderID)
	if err != nil {
		return nil, err
	}
	if !found {
		if !config.CreateNamespace {
			return nil, errors.New("Suchi Drive namespace is missing under the configured parent; create the marked namespace before starting")
		}
		namespaceID, err = drive.createNamespace(ctx, config.FolderID)
		if err != nil {
			return nil, err
		}
	}
	return &Store{drive: drive, namespaceID: namespaceID}, nil
}

func (d *driveClient) validateParent(ctx context.Context, parentID string) error {
	query := url.Values{
		"fields":            []string{"id,mimeType,trashed"},
		"supportsAllDrives": []string{"true"},
	}
	var parent driveFile
	if err := d.getJSON(ctx, d.apiFileURL(parentID, query), &parent); err != nil {
		var statusErr apiStatusError
		if !errors.As(err, &statusErr) || statusErr.status != http.StatusNotFound {
			return fmt.Errorf("read configured Google Drive parent folder: %w", err)
		}
		if childErr := d.validateParentThroughChildren(ctx, parentID); childErr != nil {
			return fmt.Errorf("configured Google Drive parent metadata is unavailable; verify the folder ID and owner/app authorization: %w", childErr)
		}
		return nil
	}
	if parent.ID != parentID || parent.MIMEType != folderMIMEType || parent.Trashed {
		return errors.New("configured Google Drive parent is missing, trashed, or not a folder")
	}
	return nil
}

// validateParentThroughChildren supports drive.file grants that can list a
// parent's app-created children while hiding the parent metadata.
func (d *driveClient) validateParentThroughChildren(ctx context.Context, parentID string) error {
	parentQuery := escapeDriveQuery(parentID)
	aetherQuery := fmt.Sprintf("'%s' in parents and appProperties has { key='%s' } and trashed = false",
		parentQuery, aetherStorageKey)
	aetherFiles, err := d.listFiles(ctx, aetherQuery)
	if err != nil {
		return fmt.Errorf("list marked Aether children to verify the parent: %w", err)
	}
	if len(aetherFiles) > 0 {
		for _, file := range aetherFiles {
			if file.ID == "" || file.AppProperties[aetherStorageKey] == "" || file.Trashed ||
				!hasParent(file, parentID) || !isBinaryDriveFile(file.MIMEType) {
				return errors.New("marked Aether child metadata is malformed or outside the configured parent")
			}
		}
		return nil
	}

	namespaceQuery := fmt.Sprintf("'%s' in parents and appProperties has { key='%s' and value='%s' } and trashed = false",
		parentQuery, namespaceProperty, namespacePropertyValue)
	namespaceFiles, err := d.listFiles(ctx, namespaceQuery)
	if err != nil {
		return fmt.Errorf("list marked Suchi namespace children to verify the parent: %w", err)
	}
	if len(namespaceFiles) == 0 {
		return errors.New("no marked Aether file or Suchi namespace is visible directly under the parent")
	}
	for _, file := range namespaceFiles {
		if file.ID == "" || file.AppProperties[namespaceProperty] != namespacePropertyValue ||
			file.MIMEType != folderMIMEType || file.Trashed || !hasParent(file, parentID) {
			return errors.New("marked Suchi namespace metadata is malformed or outside the configured parent")
		}
	}
	return nil
}

func isBinaryDriveFile(mimeType string) bool {
	return mimeType != "" && mimeType != folderMIMEType && !strings.HasPrefix(mimeType, "application/vnd.google-apps.")
}

func (d *driveClient) listFiles(ctx context.Context, q string) ([]driveFile, error) {
	var all []driveFile
	seenTokens := make(map[string]struct{})
	pageToken := ""
	for {
		query := url.Values{
			"q":                         []string{q},
			"pageSize":                  []string{"1000"},
			"supportsAllDrives":         []string{"true"},
			"includeItemsFromAllDrives": []string{"true"},
			"fields":                    []string{"nextPageToken,files(id,name,mimeType,parents,trashed,appProperties,size,sha256Checksum,modifiedTime)"},
		}
		if pageToken != "" {
			query.Set("pageToken", pageToken)
		}
		var page filePage
		if err := d.getJSON(ctx, d.apiURL("/files", query), &page); err != nil {
			return nil, fmt.Errorf("list Google Drive files: %w", err)
		}
		all = append(all, page.Files...)
		if page.NextPageToken == "" {
			return all, nil
		}
		if _, exists := seenTokens[page.NextPageToken]; exists {
			return nil, errors.New("Google Drive repeated a page token")
		}
		seenTokens[page.NextPageToken] = struct{}{}
		pageToken = page.NextPageToken
	}
}

func (d *driveClient) findNamespace(ctx context.Context, parentID string) (string, bool, error) {
	q := fmt.Sprintf("'%s' in parents and appProperties has { key='%s' and value='%s' } and trashed = false",
		escapeDriveQuery(parentID), namespaceProperty, namespacePropertyValue)
	files, err := d.listFiles(ctx, q)
	if err != nil {
		return "", false, err
	}
	var found *driveFile
	for i := range files {
		file := &files[i]
		if file.AppProperties[namespaceProperty] != namespacePropertyValue || file.Trashed {
			continue
		}
		if file.MIMEType != folderMIMEType || !hasParent(*file, parentID) || file.ID == "" {
			return "", false, errors.New("marked Suchi Drive namespace is malformed or outside the configured parent")
		}
		if found != nil {
			return "", false, errors.New("multiple marked Suchi Drive namespaces exist under the configured parent")
		}
		found = file
	}
	if found == nil {
		return "", false, nil
	}
	return found.ID, true, nil
}

func (d *driveClient) createNamespace(ctx context.Context, parentID string) (string, error) {
	metadata := map[string]any{
		"name":     "suchi-blobs",
		"mimeType": folderMIMEType,
		"parents":  []string{parentID},
		"appProperties": map[string]string{
			namespaceProperty: namespacePropertyValue,
		},
	}
	body, _ := json.Marshal(metadata)
	query := url.Values{
		"fields":            []string{"id,name,mimeType,parents,appProperties,trashed"},
		"supportsAllDrives": []string{"true"},
	}
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	resp, createErr := d.request(ctx, http.MethodPost, d.apiURL("/files", query), bytes.NewReader(body), headers)
	if createErr == nil {
		_ = resp.Body.Close()
	}

	// Re-query after both success and an ambiguous response. If the create was
	// accepted but its response was lost, this avoids creating a second folder.
	id, found, lookupErr := d.findNamespace(ctx, parentID)
	if lookupErr == nil && found {
		return id, nil
	}
	if createErr != nil {
		return "", fmt.Errorf("create Suchi Drive namespace: %w", createErr)
	}
	if lookupErr != nil {
		return "", fmt.Errorf("verify created Suchi Drive namespace: %w", lookupErr)
	}
	return "", errors.New("Google Drive did not return the created Suchi namespace")
}

func (s *Store) lookupBlob(ctx context.Context, sum string) (driveFile, bool, error) {
	q := fmt.Sprintf("'%s' in parents and name = '%s' and appProperties has { key='%s' and value='%s' } and trashed = false",
		escapeDriveQuery(s.namespaceID), sum, blobProperty, sum)
	files, err := s.drive.listFiles(ctx, q)
	if err != nil {
		return driveFile{}, false, err
	}
	switch len(files) {
	case 0:
		return driveFile{}, false, nil
	case 1:
		if err := s.validateBlobFile(sum, files[0]); err != nil {
			return driveFile{}, false, err
		}
		return files[0], true, nil
	default:
		return driveFile{}, false, fmt.Errorf("multiple Google Drive blobs match SHA-256 %s", sum)
	}
}

func (s *Store) validateBlobFile(sum string, file driveFile) error {
	if file.ID == "" || file.Name != sum || file.AppProperties[blobProperty] != sum ||
		file.Trashed || !hasParent(file, s.namespaceID) {
		return errors.New("Google Drive blob metadata does not match the owned namespace and hash")
	}
	if _, err := file.size(); err != nil {
		return err
	}
	if _, err := file.modified(); err != nil {
		return err
	}
	if file.SHA256 != "" && !checksumMatches(file.SHA256, sum) {
		return errors.New("Google Drive blob checksum metadata does not match its SHA-256 name")
	}
	return nil
}

func (s *Store) ownedBlobs(ctx context.Context) ([]driveFile, error) {
	q := fmt.Sprintf("'%s' in parents and appProperties has { key='%s' } and trashed = false",
		escapeDriveQuery(s.namespaceID), blobProperty)
	files, err := s.drive.listFiles(ctx, q)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(files))
	for _, file := range files {
		sum := file.AppProperties[blobProperty]
		if !validHash(sum) {
			return nil, errors.New("Google Drive Suchi blob has an invalid SHA-256 app property")
		}
		if err := s.validateBlobFile(sum, file); err != nil {
			return nil, err
		}
		if _, exists := seen[sum]; exists {
			return nil, fmt.Errorf("duplicate Google Drive blobs exist for SHA-256 %s", sum)
		}
		seen[sum] = struct{}{}
	}
	return files, nil
}

func validHash(sum string) bool {
	if len(sum) != 64 {
		return false
	}
	for i := 0; i < len(sum); i++ {
		c := sum[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
