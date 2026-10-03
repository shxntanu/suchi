// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"syscall"

	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/blob/gdrive"
	"github.com/johnnybravo-xyz/suchi/core/config"
	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/gc"
	"github.com/johnnybravo-xyz/suchi/core/importer/aether"
	"github.com/johnnybravo-xyz/suchi/core/logx"
	"github.com/johnnybravo-xyz/suchi/distro/internal/storage"
)

func runMigrateAether(args []string) int {
	fs := flag.NewFlagSet("suchi migrate-aether", flag.ContinueOnError)
	out := fs.String("out", "", "new output bundle directory (required)")
	dry := fs.Bool("dry-run", false, "verify source and report projected bytes without publishing a bundle")
	if err := fs.Parse(args); err != nil {
		return flagParseExit(err)
	}
	if fs.NArg() != 0 || *out == "" {
		fmt.Fprintln(os.Stderr, "--out is required; positional arguments are unsupported")
		return 2
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	source, err := gdrive.NewSource(ctx, storage.DriveConfig(cfg, false))
	if err != nil {
		fmt.Fprintln(os.Stderr, "Aether Drive source:", err)
		return 1
	}
	report, err := aether.Run(ctx, source, aether.Options{OutputDir: *out, DryRun: *dry})
	if report != nil {
		if outputErr := json.NewEncoder(os.Stdout).Encode(report); outputErr != nil {
			fmt.Fprintln(os.Stderr, "report:", outputErr)
			return 1
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "migration:", err)
		return 1
	}
	return 0
}

func runStorageTransfer(args []string) int {
	fs := flag.NewFlagSet("suchi storage-transfer", flag.ContinueOnError)
	apply := fs.Bool("apply", false, "publish verified local blobs to Drive; all archive writers must be stopped")
	if err := fs.Parse(args); err != nil {
		return flagParseExit(err)
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "positional arguments are unsupported")
		return 2
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	if cfg.StorageProvider != "gdrive" {
		fmt.Fprintln(os.Stderr, "storage-transfer requires STORAGE_PROVIDER=gdrive")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// Require an existing catalog; a typo must not create a new empty archive.
	dbPath := filepath.Join(cfg.DataDir, "suchi.db")
	if _, err := os.Stat(dbPath); err != nil {
		fmt.Fprintln(os.Stderr, "existing catalog:", err)
		return 1
	}
	d, err := db.Open(ctx, dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "catalog:", err)
		return 1
	}
	defer d.Close()
	if err := migrations.Prepare(ctx, d, logx.Setup(os.Stderr, cfg.LogLevel)); err != nil {
		fmt.Fprintln(os.Stderr, "catalog preparation:", err)
		return 1
	}
	refs, err := gc.CollectReferences(ctx, d)
	if err != nil {
		fmt.Fprintln(os.Stderr, "references:", err)
		return 1
	}
	local, err := blob.New(cfg.DataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "local CAS:", err)
		return 1
	}
	var remote *blob.CAS
	if *apply {
		fmt.Fprintln(os.Stderr, "All archive writers must remain stopped during storage-transfer.")
		remote, err = storage.New(ctx, cfg, true)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Drive CAS:", err)
			return 1
		}
	}
	var hashes []string
	for sum := range refs {
		hashes = append(hashes, sum)
	}
	sort.Strings(hashes)
	var bytes int64
	for _, sum := range hashes {
		n, err := transferBlob(ctx, local, remote, sum)
		if err != nil {
			fmt.Fprintf(os.Stderr, "blob %s: %v\n", sum, err)
			return 1
		}
		bytes += n
	}
	if err := json.NewEncoder(os.Stdout).Encode(struct {
		Apply bool  `json:"apply"`
		Blobs int   `json:"blobs"`
		Bytes int64 `json:"bytes"`
	}{*apply, len(hashes), bytes}); err != nil {
		fmt.Fprintln(os.Stderr, "report:", err)
		return 1
	}
	return 0
}

func transferBlob(ctx context.Context, local, remote *blob.CAS, sum string) (int64, error) {
	r, err := local.GetContext(ctx, sum)
	if err != nil {
		return 0, err
	}
	defer r.Close()
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return 0, err
	}
	if hex.EncodeToString(h.Sum(nil)) != sum {
		return 0, fmt.Errorf("local content failed SHA-256 verification")
	}
	if remote == nil {
		return n, nil
	}
	seeker, ok := r.(io.Seeker)
	if !ok {
		return 0, fmt.Errorf("local CAS reader is not seekable")
	}
	if _, err := seeker.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	ref, err := remote.PutContext(ctx, r)
	if err != nil {
		return 0, err
	}
	if ref.SHA256 != sum || ref.Size != n {
		return 0, fmt.Errorf("destination reference mismatch")
	}
	return n, nil
}
