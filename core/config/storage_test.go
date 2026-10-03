// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"strings"
	"testing"
)

func TestStorageConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, provider, render string
		credentials, demo      bool
		wantRender             bool
		wantError              string
	}{
		{name: "local default", wantRender: true},
		{name: "drive default", provider: "gdrive", credentials: true},
		{name: "drive views", provider: "gdrive", credentials: true, render: "true", wantRender: true},
		{name: "local views disabled", render: "false"},
		{name: "partial drive", provider: "gdrive", wantError: "GDRIVE"},
		{name: "unknown", provider: "s3", wantError: "STORAGE_PROVIDER"},
		{name: "invalid bool", render: "maybe", wantError: "RENDER_DOCUMENT_VIEWS"},
		{name: "demo drive", provider: "gdrive", credentials: true, demo: true, wantError: "demo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateConfigEnv(t)
			t.Setenv("PUBLIC_URL", "http://localhost")
			t.Setenv("STORAGE_PROVIDER", tc.provider)
			t.Setenv("RENDER_DOCUMENT_VIEWS", tc.render)
			for _, name := range []string{"GDRIVE_CLIENT_ID", "GDRIVE_CLIENT_SECRET", "GDRIVE_REFRESH_TOKEN", "GDRIVE_FOLDER_ID"} {
				value := ""
				if tc.credentials {
					value = "test-value"
				}
				t.Setenv(name, value)
				t.Setenv(name+"_FILE", "")
			}
			if tc.demo {
				t.Setenv("SUCHI_DEMO_MODE", "true")
			}
			cfg, err := Load()
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("error = %v, want %s", err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.RenderDocumentViews != tc.wantRender {
				t.Fatalf("views = %v, want %v", cfg.RenderDocumentViews, tc.wantRender)
			}
			want := tc.provider
			if want == "" {
				want = "local"
			}
			if cfg.StorageProvider != want {
				t.Fatalf("provider = %s", cfg.StorageProvider)
			}
		})
	}
}
