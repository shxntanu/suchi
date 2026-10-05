# Render

Suchi can use Turso for remote SQL metadata while keeping local SQLite as the
default for ordinary deployments. For Render, create the web service from the
Suchi image or repository and configure its variables under the service's
Environment settings:

- `PUBLIC_URL` — the service's public HTTPS origin.
- `LISTEN_ADDR` — bind to `0.0.0.0:10000` for Render's default web-service port.
- `TURSO_DATABASE_URL` — `libsql://suchi-shxntanu.aws-ap-south-1.turso.io`.
- `TURSO_AUTH_TOKEN` — the database token, stored as a Render secret. You can
  also mount it as a Render Secret File and set `TURSO_AUTH_TOKEN_FILE` to its
  path.
- `DATA_DIR` — the local data directory (for example `/data`).

Render does not automatically load `.env`, `.env.prod`, or another dotenv file.
Set values in the service environment; enter `TURSO_AUTH_TOKEN` through the
secret field. Do not commit a token or put secrets in a `render.yaml`.

Remote mode sends SQL metadata directly to Turso through the Go
`database/sql` client. It does not use an embedded replica. Keep one Suchi
service process: Suchi serializes writes through one writer, and multiple
Suchi writers are not coordinated by Turso. The Go remote client is deprecated
upstream but currently provides this direct remote route; replacing that
client in future is an implementation concern.

Turso stores only SQL metadata. It does not persist CAS blobs, `.decrypt-key`,
rendered views, staging files, or working files. Render Free's filesystem is
ephemeral, so restarts or instance replacement can lose those files even while
Turso metadata remains available. With Suchi's current storage, Turso alone
does not make Render Free safe for user documents. Before storing an archive,
provide an external durable object/file strategy for document bytes and a
durable, recoverable strategy for `.decrypt-key` and other required local
state, or use a host with persistent storage. Suchi's optional Google Drive
blob storage can hold document bytes, but does not persist the credential key
or working files. Keep staging and temporary capacity in mind for uploads and
ingest.

For database and archive recovery requirements, see
[Backup and restore](../../docs/backup-restore.mdx). For all boot variables, see
[Configuration](../../docs/config.mdx).
