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

**Files:** Create `core/blob/gdrive/*.go`. No changes outside this directory.

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

- [x] Omit new unit tests as explicitly requested; use existing suites and manual checks.
- [x] Compile the new package and record existing-suite verification in the task report.
- [x] Implement auth/namespace/read inventory first; commit this complete stage.
- [x] Implement resumable uploads, integrity and bounded retry behavior; commit this stage.
- [x] Run focused tests, race tests, gofmt and vet; report commits, verification and limitations.

## Task 2: Remote CAS and maintenance (parallel lane B)

**Files:** Modify/create `core/blob/*.go`, `core/gc/gc.go`, existing test call signatures only.

**Interfaces:** Consume `RemoteBackend`. Produce `NewRemote(dir string, backend RemoteBackend) (*CAS,error)`;
`PutContext(context.Context,io.Reader) (pluginapi.BlobRef,error)`;
`GetContext(context.Context,string) (io.ReadCloser,error)`;
`StatContext(context.Context,string) (pluginapi.BlobRef,error)`;
`DeleteContext(context.Context,string) error`;
`ListContext(context.Context,func(RemoteObject) error) error`;
`MaterializeContext(context.Context,string) (string,error)`;
`IsRemote() bool`. Existing methods call context variants with Background and preserve local semantics.
`Path` stays pure. Remote Get returns a seekable reader cleaned up on Close.

- [x] Omit new unit tests as explicitly requested; use existing suites and manual checks.
- [x] Compile the new package and record existing-suite verification in the task report.
- [x] Implement remote Put/Get/Stat and temporary-file lifecycle; commit the stage.
- [x] Implement remote List/Delete/Scrub and adapt GC grace to backend modification times. Reject remote quarantine before mutation. Commit maintenance stage.
- [x] Verify blob/GC tests, concurrency with race, gofmt and vet. Report staged commits and concerns.

## Task 3: Aether-to-bundle converter (parallel lane C)

**Files:** Create `core/importer/aether/*.go` only.

**Interfaces:** Produce `Source` interface with `List(context.Context) (map[string]string,error)` and
`Open(context.Context,string) (io.ReadCloser,error)`; compatible with Task 1's source.
Produce `Options{OutputDir string, DryRun bool}`, `Report` containing document/tag/skipped/failure
counts, `ProjectedBytes int64`, mapping records and diagnostics; and
`Run(context.Context,Source,Options) (*Report,error)`.

- [x] Omit new unit tests as explicitly requested; use existing suites and manual checks.
- [x] Compile the new package and record existing-suite verification in the task report.
- [x] Implement read-only inventory and dry-run projected-byte report; commit stage.
- [x] Implement exclusive output bundle publication and mapping report; commit stage.
- [x] Verify produced bundles with existing bundle loader/importer tests, gofmt and vet; report commits and limitations.

## Task 4: Runtime configuration, rendering and CLI integration (controller)

**Files:** `core/config/`, `distro/internal/storage/`, `distro/app/`, `distro/cmd/suchi/`,
remote consumer callsites in `core/api/`, `core/ui/`, `core/pipeline/postingest/`,
`core/ingest/`, `core/importer/bundle/`, `core/render/view/`, `distro/internal/diagnostics/`.

**Interfaces:** One `storage.New(ctx,cfg,createNamespace) (*blob.CAS,error)` constructor
uses Task 1 and 2. Config adds provider/Drive fields and resolved `RenderDocumentViews`.
CLI adds `suchi migrate-aether --out <dir> [--dry-run]` using Task 3;
and `suchi storage-transfer --apply` for stopped-writer local-to-Drive publication.

- [x] Omit new unit tests as explicitly requested; use existing suites and manual checks.
- [x] Wire storage creation in server and all CLI producers/consumers; use request/job contexts.
- [x] Handle disabled document render jobs deliberately, preserve taxonomy index; materialize opt-in link targets outside write transactions.
- [x] Add migration and transfer CLI contracts: dry-run counts, safe explicit apply, source verification, no accidental local fallback and no secret output.
- [x] Add egress/doctor reporting and source-safe GC/scrub. Existing local consumer suites and manual adapter/converter smoke checks cover the available offline validation; live Drive requests remain blocked by DNS.
- [x] Commit integration in stages, then run affected tests and race tests.

## Task 5: Documentation, review and verification

**Files:** `.env.sample`, `CHANGELOG.md`, `docs/config.mdx`, `docs/architecture.mdx`,
`docs/backup-restore.mdx`, `docs/cli.mdx`, `deploy/README.md`, new Drive runbook.

- [x] Document exact credential mapping, separate namespaces, physical-view disk requirements, complete backups, migration/rollback and working-file latency.
- [x] Run `make check` and `make smoke`; report missing tools/network dependencies rather than bypassing failed checks.
- [x] Independently review each task and the integrated diff; fix material findings with existing checks and manual verification. No new unit tests.
- [x] Attempt read-only live compatibility probe if network permits, never log secrets or count mocks as live success.
- [x] Commit final documentation and fixes; leave feature branch reviewable without publishing or merging.

## Verification outcome

The production binary builds. Existing focused suites and race checks pass. All-module vet, formatting, license checks and documentation metadata checks pass. `make check` was attempted: localhost listeners are forbidden by the sandbox, causing existing API/mail/app/CLI/demo/OIDC/classifier tests to fail. `make smoke` built and reached server startup but failed at the forbidden localhost bind. Staticcheck could not be downloaded because DNS is blocked; UI checks require unavailable Bun. Live OAuth refresh failed at DNS before authorization, so the existing Aether credentials remain unverified. No real Drive data was changed or migrated.
