# Google Drive Storage Implementation Plan

> **For agentic workers:** Use superpowers:subagent-driven-development and independent parallel worktrees. User selected gpt-6-luna, max effort, parallel implementation and staged commits.

**Goal:** Host Suchi with immutable document bytes in Google Drive, a persistent local catalog, and a safe Aether migration path.

**Architecture:** Preserve the CAS facade and add a remote backend contract. A Drive adapter, temporary seekable remote CAS, and Aether bundle converter are independent deliverables; application wiring follows their stable interfaces.

**Tech Stack:** Go 1.27, standard HTTP/JSON/filesystem libraries, existing oauth2, SQLite.

**Spec:** `docs/superpowers/specs/2026-10-03-google-drive-storage-design.md`

## Global Constraints

- Latest user steering: do not add unit tests. Remove newly added unit tests; validate with existing tests, build/vet and runtime checks. This supersedes every test-creation step below.

- Default local behavior remains compatible; no new outbound connection in local mode.
- One process owns an archive. Network I/O stays outside writer transactions.
- Never copy Aether source or credentials into Suchi; all new source has AGPL SPDX headers.
- Only Suchi's marked child namespace is mutable. Source Aether files are read-only.
- Use context-aware I/O, verified immutable SHA-256 bytes and seekable downloads.
- Physical document views default off for Drive; optional materialization retains full targets.
- User authorizes staged commits, parallel isolated worktrees, and execution without another planning handoff.
- Tests must exercise behavior. Do not claim live credential verification without real successful requests.

## Review Focus

- An upload accepted remotely followed by lost response must not create or overwrite inconsistent bytes.
- Drive duplicates and pagination must not cause arbitrary selection or incomplete inventory.
- A cache/temp cleanup failure must not remove operator files, keys or durable local originals.
- Trash/purge must preserve shared remote blobs; GC must exclude Aether objects.
- Migration must report missing/corrupt files, hostile filenames and integer-ID collisions.

## Task 1: Drive adapter and source reader (parallel lane A)

**Files:** Create `core/blob/gdrive/*.go` and tests. No changes outside this directory.

**Interfaces:** Implement the predeclared `blob.RemoteBackend` in `remote_contract.go`:
`Put(ctx, sum, io.ReadSeeker, size) error`, `Get(ctx,sum) (io.ReadCloser,error)`,
`Stat(ctx,sum) (blob.RemoteObject,error)`, `List(ctx,fn func(blob.RemoteObject) error) error`,
`Delete(ctx,sum) error`. Missing returns `blob.ErrNotFound`.

Produce `gdrive.Config` with `ClientID`, `ClientSecret`, `RefreshToken`, `FolderID` strings;
`HTTPClient *http.Client`, `APIBaseURL`, `UploadBaseURL`, `TokenURL` strings, and
`CreateNamespace bool`. Produce `New(context.Context,Config) (*Store,error)`.
`NewSource(context.Context,Config) (*Source,error)` is read-only and has
`List(context.Context) (map[string]string,error)` returning Aether storage key to file ID,
and `Open(context.Context,string) (io.ReadCloser,error)` opening a listed file ID.

- [ ] Write failing fake-HTTP tests for OAuth, parent validation, marked child folder, immutable dedup, checksums, resumable uploads including empty bytes, offsets/retries, hostile upload URLs, read/list/delete, pagination, duplicate keys and secret redaction.
- [ ] Run `go test ./core/blob/gdrive` and record the expected initial failure.
- [ ] Implement auth/namespace/read inventory first; commit this complete stage.
- [ ] Implement resumable uploads, integrity and bounded retry behavior; commit this stage.
- [ ] Run focused tests, race tests, gofmt and vet; report commits, verification and limitations.

## Task 2: Remote CAS and maintenance (parallel lane B)

**Files:** Modify/create `core/blob/*.go`, `core/gc/gc.go`, affected tests only.

**Interfaces:** Consume `RemoteBackend`. Produce `NewRemote(dir string, backend RemoteBackend) (*CAS,error)`;
`PutContext(context.Context,io.Reader) (pluginapi.BlobRef,error)`;
`GetContext(context.Context,string) (io.ReadCloser,error)`;
`StatContext(context.Context,string) (pluginapi.BlobRef,error)`;
`DeleteContext(context.Context,string) error`;
`ListContext(context.Context,func(RemoteObject) error) error`;
`MaterializeContext(context.Context,string) (string,error)`;
`IsRemote() bool`. Existing methods call context variants with Background and preserve local semantics.
`Path` stays pure. Remote Get returns a seekable reader cleaned up on Close.

- [ ] Write failing backend-fake tests for hashing, remote commit before success, seekability, checksum failures, cancellation, temp cleanup, local compatibility and materialization.
- [ ] Run `go test ./core/blob ./core/gc` and record the initial contract failure.
- [ ] Implement remote Put/Get/Stat and temporary-file lifecycle; commit the stage.
- [ ] Implement remote List/Delete/Scrub and adapt GC grace to backend modification times. Reject remote quarantine before mutation. Commit maintenance stage.
- [ ] Verify blob/GC tests, concurrency with race, gofmt and vet. Report staged commits and concerns.

## Task 3: Aether-to-bundle converter (parallel lane C)

**Files:** Create `core/importer/aether/*.go` and tests only.

**Interfaces:** Produce `Source` interface with `List(context.Context) (map[string]string,error)` and
`Open(context.Context,string) (io.ReadCloser,error)`; compatible with Task 1's source.
Produce `Options{OutputDir string, DryRun bool}`, `Report` containing document/tag/skipped/failure
counts, `ProjectedBytes int64`, mapping records and diagnostics; and
`Run(context.Context,Source,Options) (*Report,error)`.

- [ ] Write failing fixture tests for valid v1 Aether sidecars, original pairing, checksum/size verification, tags/title/filename/timestamps, skipped non-ready states, malformed UUID/schema, missing manifest/original, traversal, refusal to overwrite, cancellation, deterministic identifiers/collision handling and importer compatibility.
- [ ] Run focused tests and record expected initial failures.
- [ ] Implement read-only inventory and dry-run projected-byte report; commit stage.
- [ ] Implement exclusive output bundle publication and mapping report; commit stage.
- [ ] Verify produced bundles with existing bundle loader/importer tests, gofmt and vet; report commits and limitations.

## Task 4: Runtime configuration, rendering and CLI integration (controller)

**Files:** `core/config/`, `distro/internal/storage/`, `distro/app/`, `distro/cmd/suchi/`,
remote consumer callsites in `core/api/`, `core/ui/`, `core/pipeline/postingest/`,
`core/ingest/`, `core/importer/bundle/`, `core/render/view/`, `distro/internal/diagnostics/`.

**Interfaces:** One `storage.New(ctx,cfg,createNamespace) (*blob.CAS,error)` constructor
uses Task 1 and 2. Config adds provider/Drive fields and resolved `RenderDocumentViews`.
CLI adds `suchi migrate-aether --out <dir> [--dry-run]` using Task 3;
and `suchi storage-transfer --apply` for stopped-writer local-to-Drive publication.

- [ ] Add failing configuration tests for default/provider validation, incomplete secrets and rendering defaults.
- [ ] Wire storage creation in server and all CLI producers/consumers; use request/job contexts.
- [ ] Handle disabled document render jobs deliberately, preserve taxonomy index; materialize opt-in link targets outside write transactions.
- [ ] Add migration and transfer CLI contracts: dry-run counts, safe explicit apply, source verification, no accidental local fallback and no secret output.
- [ ] Add egress/doctor reporting, source-safe GC/scrub behavior and end-to-end fake-Drive upload/download/range/export checks.
- [ ] Commit integration in stages, then run affected tests and race tests.

## Task 5: Documentation, review and verification

**Files:** `.env.sample`, `CHANGELOG.md`, `docs/config.mdx`, `docs/architecture.mdx`,
`docs/backup-restore.mdx`, `docs/cli.mdx`, `deploy/README.md`, new Drive runbook.

- [ ] Document exact credential mapping, separate namespaces, physical-view disk requirements, complete backups, migration/rollback and working-file latency.
- [ ] Run `make check` and `make smoke`; report missing tools/network dependencies rather than bypassing failed checks.
- [ ] Independently review each task and the integrated diff; fix material findings with regression coverage.
- [ ] Attempt read-only live compatibility probe if network permits, never log secrets or count mocks as live success.
- [ ] Commit final documentation and fixes; leave feature branch reviewable without publishing or merging.
