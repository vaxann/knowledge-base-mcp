## Purpose

Lets people and clients move non-Markdown files (PDF, images, Office documents) into and out of the vault: read them through MCP, upload them, and exchange them with a browser or phone through short-lived signed links.

## ADDED Requirements

### Requirement: Read any vault file through MCP
The server SHALL provide `kb_get_file(path, max_bytes)` returning metadata (path, kind, media type, size, modified, etag) and the file bytes as an embedded binary resource with the right media type, and the resource template `kb://file/<path>` returning the same bytes as a blob. Files larger than `max_bytes` (default 10 MB) SHALL fail with `too_large` while still returning the metadata, pointing the client to `kb_file_link`.

#### Scenario: PDF returned inline
- **WHEN** a client calls `kb_get_file` on a 200 KB PDF
- **THEN** the result contains an embedded resource with media type `application/pdf` and the exact bytes, plus the metadata

#### Scenario: Too large
- **WHEN** the file exceeds `max_bytes`
- **THEN** the call fails with `too_large` and the payload carries the file metadata

### Requirement: Upload any file through MCP
The server SHALL provide `kb_upload_file(path, content_base64, overwrite, summary)` that stores the decoded bytes at any visible path (creating folders) and commits it as `kb_upload_file: <path>`; an existing path fails with `already_exists` unless `overwrite` is set. Uploads above the configured limit (`KB_MAX_UPLOAD`, default 50 MB) fail with `too_large`. `kb_move_note` and `kb_delete_note` SHALL accept attachments as well as notes; a move MUST NOT turn a note into an attachment or vice versa.

#### Scenario: Upload and commit
- **WHEN** a client uploads `Files/scan.pdf` from base64
- **THEN** the file exists in the work tree, is listed as an attachment, and exactly one commit records it

#### Scenario: Move and delete an attachment
- **WHEN** a client moves `Files/scan.pdf` to `Archive/scan.pdf` and then deletes it
- **THEN** each step is one commit and the file can still be read from history

### Requirement: Signed download links
The server SHALL provide `kb_file_link(path, ttl_minutes)` returning an HTTPS URL on the public base (`<public>/files/<path>?exp=&sig=`) that serves the file in a browser without signing in until it expires (default 15 minutes). The signature SHALL be an HMAC bound to the path and expiry with a server-side key persisted next to the index; tampering, another path, or expiry MUST yield `403`. Files SHALL be served with their media type, a filename, `nosniff` and a sandboxing Content-Security-Policy. The same route SHALL also accept a valid bearer or OAuth token instead of a signature.

#### Scenario: Download from a phone
- **WHEN** the user opens the link produced by `kb_file_link` for a PDF
- **THEN** the browser receives the PDF with the right filename, and the same link answers `403` after it expires

#### Scenario: Link bound to one file
- **WHEN** the query string of a valid link is reused with another path
- **THEN** the response is `403`

### Requirement: Signed upload links and page
The server SHALL provide `kb_upload_link(path, ttl_minutes)` returning an HTTPS URL to an upload page (`<public>/upload/<path>?exp=&sig=`). When `path` ends with `/` the page accepts several files and keeps their own base names inside that folder; otherwise the uploaded file is stored at exactly that path. A valid link or token SHALL also allow `PUT /files/<path>` with the raw bytes. Every stored upload SHALL be committed at once and reported back on the page. Expired or tampered links MUST NOT render the page nor accept files. Uploaded file names MUST be reduced to their base name so they cannot escape the folder.

#### Scenario: Upload a photo from the Claude app
- **WHEN** the assistant returns an upload link for `Inbox/` and the user picks `photo.jpg` on the page
- **THEN** `Inbox/photo.jpg` is committed and the page confirms the stored path

#### Scenario: Expired upload link
- **WHEN** the link is opened after its expiry
- **THEN** the page is not shown and the response is `403`

### Requirement: Links require a public URL
Signed links SHALL be built on `KB_PUBLIC_URL` (or the loopback listen address when that is the only transport); in stdio mode `kb_file_link` and `kb_upload_link` SHALL fail with `invalid_argument` explaining that the HTTP transport and a public URL are needed.

#### Scenario: stdio mode
- **WHEN** `kb_file_link` is called on a stdio-only server
- **THEN** it fails with `invalid_argument`
