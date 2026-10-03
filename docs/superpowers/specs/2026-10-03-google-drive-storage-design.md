# Google Drive storage for Suchi

Status: proposed implementation design, awaiting review.

## Intent and agreed scope

Run Suchi with Google Drive holding original and derived document bytes,
while a single server keeps SQLite and credential keys on persistent disk.
Preserve useful local operation when the filesystem provider is selected.
Reuse the user's existing Aether Drive OAuth client and refresh token where
they remain valid. Render, Cloudflare, Supabase hosting configuration and
deployment are outside this change.

The user approved this storage direction on 2026-10-03. This document makes
the implementation choices and filesystem limitations explicit for review.
Actual migration of personal documents is an operator action after the
implementation is reviewed and verified.

## Findings from the current checkouts

- Aether's adapter uses Drive v3, owner OAuth with `drive.file`, resumable
  uploads and `aetherStorageKey` private app properties. Its catalog is
  PostgreSQL; Drive manifests contain document metadata and tags.
- Suchi uses a concrete `*blob.CAS`. Uploads, derived outputs and avatars
  write through it. Downloads require seekable readers. Filesystem views
  use `CAS.Path` as symlink targets. Export/import, doctor and GC construct
  their own CAS instances.
- SQLite, FTS, jobs, users, permissions, settings and `.decrypt-key` remain
  local durable state. Drive object storage does not replace these.
- Aether `.env.prod` and `.env.local` both contain all four required Drive
  credential/folder settings. No secret values were printed or copied.
- A read-only live credential probe could not reach Google's OAuth endpoint:
  DNS resolution failed in this execution environment. Token validity,
  folder access and write capability are therefore unverified. No Drive
  files have been changed.

## Architecture

Retain `*blob.CAS` as the application facade and the current `New(dir)`
filesystem behavior. Add an explicitly configured remote backend inside
the facade rather than replacing every consumer with a new plugin system.
Implement the Google adapter independently under `core/blob/gdrive/`,
using the existing `golang.org/x/oauth2` dependency and standard HTTP/JSON.
Write this adapter for Suchi's immutable hash-based contract; do not copy
Aether source into Suchi. Suchi's contributor rules restrict outside source
incorporation and its adapter contract differs from Aether's.

One process owns an archive and its Drive namespace. Do not support multiple
servers writing to the same archive. Network I/O stays outside SQLite write
transactions. All remote operations receive cancellation contexts; update
callers to use context-aware CAS entry points while retaining the existing
local constructor and local contract.

### Configuration and existing credentials

Add these environment/config-file settings:

| Setting | Behavior |
| --- | --- |
| `STORAGE_PROVIDER` | `local` by default; accepts `local` or `gdrive` |
| `GDRIVE_CLIENT_ID` | Existing Aether Drive client ID |
| `GDRIVE_CLIENT_SECRET` | Secret paired with that client |
| `GDRIVE_REFRESH_TOKEN` | Existing Aether owner's refresh token |
| `GDRIVE_FOLDER_ID` | Parent folder accessible to that client |
| `RENDER_DOCUMENT_VIEWS` | Defaults true for local storage, false for Drive |

Require complete credentials in Drive mode; reject unknown providers and
partial configuration. Explicit Suchi values take precedence. Document
mapping the four `AETHER_GDRIVE_*` values to the corresponding Suchi values;
do not implicitly search sibling repositories or load their dotenv files.
An existing token does not require the old redirect URL for refresh.
Storage owner authorization is separate from Suchi user login/OIDC.

Use the same OAuth client/token combination for the initial compatibility
check and migration. `drive.file` grants access to files created or explicitly
opened/shared with the app, not arbitrary files merely because their ID is
known. If the configured parent is inaccessible, report that condition;
do not silently request full Drive scope or change its sharing settings.

### Drive namespace and immutable blobs

Under the configured parent, find or create a child folder marked with an
app property identifying the Suchi blob namespace. Do not locate it by
display name alone. Store binary objects with their full lowercase SHA-256
as the display name and `suchiBlobSHA256` as an app property. Scope every
lookup/list/delete to this child and property. Aether originals and manifests
in the parent remain untouched. Multiple marked namespace folders are an
error rather than an arbitrary selection.

Keep references in SQLite as existing hashes: no provider IDs in document
rows and no core schema change. Drive lookup returns an object ID, size and
checksums. Deduplicate immutable content; never overwrite bytes at a hash
to repair mismatched content silently. Unexpected duplicate object matches,
wrong sizes and wrong checksums are errors requiring operator diagnosis.

Uploads first spool and hash the input to a private local temporary file.
Upload through a resumable session, check acknowledged offsets and reconcile
ambiguous failures before retrying. Use bounded retries for throttling and
transient failures; propagate quota, revoked-token and permission failures.
Validate HTTPS Google upload locations and refuse off-origin credential
redirects. Verify completed size and SHA-256 when available, otherwise read
back and hash before returning success. A document/job transaction can begin
only after its original exists in Drive. Failed database publication may leave
an orphan, consistent with the existing CAS contract.

### Reads and local working files

Remote reads download into private temporary files, verify SHA-256, rewind
and return seekable readers whose Close removes the temporary file. This
preserves HTTP byte-range handling and existing processing/export behavior.
No persistent whole-archive cache is needed in the initial implementation.
Concurrent requests may download independent copies; bound operation time
with cancellation and HTTP timeouts. Document that cold reads download the
whole selected blob and require temporary space, and that Drive/API quota
and network speed affect latency. Streaming range optimization is a separate
follow-up, not an unverified promise in this implementation.

Upload/download temporary files are outside the durable local CAS tree.
Remove abandoned working files on startup only inside a dedicated owned
temporary directory, with no other archive writers running. Never remove
SQLite, keys, operator files or local-provider blob files during cleanup.

### Filing views and disk requirements

The web filing tree, categories, taxonomy index, saved views and search
remain available in Drive mode. Physical document symlink views need complete
local targets; an evicting cache cannot keep those links useful.

Drive mode defaults `RENDER_DOCUMENT_VIEWS=false` and still publishes the
small taxonomy index. Render jobs must be deliberately handled as disabled
without repeated failures, and no document symlinks are published in this
mode. Existing document projections must not be silently deleted when the
setting changes. The operator runbook explains removing old projections
offline if desired. Local mode preserves existing rendering by default.

Operators can enable physical document views explicitly. In that mode,
materialize verified blobs at canonical local CAS paths before link
publication and retain those copies. Keep `CAS.Path` as a pure, validated
path calculation used by ownership checks; use a separate context-aware
materialization operation for publication. Hydration happens outside writer
transactions and respects the existing final liveness check/journal protocol.
Rebuilding all document views can consume the archive's full displayed-byte
size locally. A small persistent disk is therefore sufficient for catalog
plus working files only when physical document views are disabled.

## Lifecycle, maintenance and recovery

Suchi Trash remains a metadata state. Do not trash a Drive blob because one
document was trashed or purged: other documents/versions may share it.
Preserve offline GC's all-writers-stopped requirement. Enumerate only owned
Suchi blobs, use remote object modification times for grace, and delete only
unreferenced owned objects. Never enumerate/delete Aether objects as GC
candidates. A remote deletion failure remains visible in the report.

Doctor reports provider, configured egress and credential/folder-check
results without secrets. Scrub must inspect remote objects rather than
mistaking an empty local tree for data loss. Read and hash remote content;
report corrupt/missing objects. Remote quarantine is unsupported and must
return a clear error before mutation. Explicitly reject unsupported maintenance
operations rather than silently falling back to local storage.

Server, import, export, doctor, GC and other archive writers must use one
configured constructor. Startup validates namespace access before serving.
Local default startup makes no new outbound calls. Demo operation must not
use production Drive storage.

Backups still require a consistent SQLite snapshot, `.decrypt-key` and
operator configuration, plus the corresponding complete Drive namespace.
Built-in database snapshots alone are incomplete. Retained old snapshots
may reference objects reclaimed by GC: back up Drive bytes independently or
retain a complete native export. Restore to an empty target with the same
namespace and keys; validate downloads, search and sealed credentials.

Provider switching is not automatic migration. Before enabling Drive for
an existing local Suchi archive, publish and verify its referenced blobs
through an explicit offline transfer operation. Do not accept a configuration
switch that leaves existing document references silently unavailable. Keep
local originals until verification; document rollback by using the retained
local archive snapshot.

## Aether migration deliverable

Implement migration as a separate task after the storage adapter works.
Provide a read-only Aether-to-bundle converter using the existing Drive
credentials. Enumerate Aether manifests by `aetherStorageKey`, pair each
manifest with its original using the storage key suffix, validate its schema,
size and SHA-256, and build Suchi's existing `manifest.json` plus `originals/`
bundle format. Preserve source UUID in a mapping report and use deterministic
integer bundle identifiers suitable for Suchi's legacy-ID deduplication.
Validate UUIDs before deriving identifiers and reject collisions.

Preserve active documents' titles, original filenames, creation timestamps
and tags, including Aether date tags. Do not copy accounts, sessions or roles;
the operator supplies an existing Suchi owner and target system to the normal
import command. Report deleted/failed/uploading records as skipped. For an
unreadable or missing manifest/original, return a failed report and do not
claim a complete migration. Leave Aether files and metadata untouched.

Use a dedicated Suchi namespace, so the first migration may duplicate
original bytes in Drive. Zero-copy adoption of Aether objects would couple
Suchi GC to Aether deletion and is outside this design. Require the migration
report to show projected Drive bytes before producing the full bundle.
Use explicit output paths, refuse overwrites, validate bundle paths and write
no credentials into bundles/reports. The operator reviews the bundle/report
before importing their real archive. Do not mutate the live Aether catalog.

## Verification and acceptance

The user requested credential compatibility verification. Implementation
verification must cover real observable behavior, not source-text checks:

- Default local behavior remains compatible and does not contact Google.
- Fake Drive tests cover OAuth refresh, folder access, owned namespace,
  resumable upload, duplicate/ambiguous uploads, pagination, integrity errors,
  token revocation, quota errors, cancellation and secret redaction.
- Remote CAS put/get/stat/list/delete contracts, seekable readers and temp
  cleanup are covered, including empty content and interrupted transfers.
- Authenticated previews/downloads/shares preserve ACLs and byte ranges.
- Originals and derived outputs survive deleting working files and restarting.
- Rendering-disabled mode completes jobs without broken symlinks; opt-in
  rendering materializes verified targets and preserves purge race guards.
- Export/import, doctor, scrub and offline GC use the configured provider;
  shared blobs and Aether objects survive Trash/purge and unrelated GC.
- Migration validates source hashes, preserves metadata, reports skipped
  records/failures, refuses path traversal and can be imported repeatedly.
- Run affected package tests, race tests for new concurrency, formatting/vet,
  `make check` and runtime smoke checks as repository rules require. Report
  dependency/network/tool limitations explicitly.
- Live credential check: refresh existing token, read parent metadata/list,
  and report access/capabilities with no secrets. Verify write/read/delete only
  in an explicitly identified disposable Suchi test namespace, leaving
  Aether objects untouched. If network access is unavailable, live validation
  remains outstanding and is not replaced by mocked success.

Update the architecture, configuration, backup/restore and deployment guides,
environment sample and changelog with the shipped behavior and limitations.
Do not commit changes unless the user asks, per Suchi's AGENTS.md.

## Primary API references

- [Drive scopes](https://developers.google.com/workspace/drive/api/guides/api-specific-auth)
- [Private app properties](https://developers.google.com/workspace/drive/api/guides/properties)
- [Files resource and checksums](https://developers.google.com/workspace/drive/api/reference/rest/v3/files)
- [Resumable uploads](https://developers.google.com/workspace/drive/api/guides/manage-uploads)
