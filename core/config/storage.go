// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

func (c *Config) loadStorage() error {
	c.StorageProvider = strings.ToLower(strings.TrimSpace(env("STORAGE_PROVIDER", "local")))
	if c.StorageProvider == "" {
		c.StorageProvider = "local"
	}
	if c.StorageProvider != "local" && c.StorageProvider != "gdrive" {
		return errors.New("STORAGE_PROVIDER: want local or gdrive")
	}
	c.RenderDocumentViews = c.StorageProvider == "local"
	if raw := env("RENDER_DOCUMENT_VIEWS", ""); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return errors.New("RENDER_DOCUMENT_VIEWS: want true or false")
		}
		c.RenderDocumentViews = value
	}
	c.GDriveClientID = strings.TrimSpace(env("GDRIVE_CLIENT_ID", ""))
	c.GDriveFolderID = strings.TrimSpace(env("GDRIVE_FOLDER_ID", ""))
	var err error
	if c.GDriveClientSecret, err = readSecret("GDRIVE_CLIENT_SECRET"); err != nil {
		return err
	}
	if c.GDriveRefreshToken, err = readSecret("GDRIVE_REFRESH_TOKEN"); err != nil {
		return err
	}
	if c.StorageProvider == "gdrive" {
		for name, value := range map[string]string{
			"GDRIVE_CLIENT_ID":     c.GDriveClientID,
			"GDRIVE_CLIENT_SECRET": c.GDriveClientSecret,
			"GDRIVE_REFRESH_TOKEN": c.GDriveRefreshToken,
			"GDRIVE_FOLDER_ID":     c.GDriveFolderID,
		} {
			if strings.TrimSpace(value) == "" {
				return fmt.Errorf("%s is required for Drive storage", name)
			}
		}
		if c.DemoMode {
			return errors.New("demo mode requires local storage")
		}
	}
	return nil
}
