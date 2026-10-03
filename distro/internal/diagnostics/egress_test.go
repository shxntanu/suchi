// SPDX-License-Identifier: AGPL-3.0-or-later

package diagnostics

import (
	"context"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/config"
	"github.com/johnnybravo-xyz/suchi/core/db"
)

func TestDriveEgressRedactsCredentials(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, t.TempDir()+"/suchi.db")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	cfg := &config.Config{StorageProvider: "gdrive", GDriveClientSecret: "secret-value", GDriveRefreshToken: "refresh-value"}
	values, _ := EnumerateEgress(ctx, d, cfg, "")
	joined := strings.Join(values, "\n")
	if !strings.Contains(joined, "storage.gdrive") || !strings.Contains(joined, "oauth2.googleapis.com") {
		t.Fatalf("Drive egress missing: %v", values)
	}
	if strings.Contains(joined, "secret-value") || strings.Contains(joined, "refresh-value") {
		t.Fatal("secret leaked")
	}
}
