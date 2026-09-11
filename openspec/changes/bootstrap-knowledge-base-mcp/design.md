## Context

See proposal.md for motivation. Constraints that shape the design:

- The vault is an Obsidian-compatible Markdown tree: YAML frontmatter, `[[wikilinks]]`, `#tags`, `_templates/`-style helper folders, many binary attachments (PDF, images) next to notes. Notes may mix Cyrillic and Latin content, so tokenisation and stemming must handle both.
- Target size: on the order of 1,000–10,000 notes, with attachments that can make the repository hundreds of MB, so the index must never scan binaries.
- The vault may be edited concurrently by a desktop editor on the same clone and by other clones pushing to the same remote.
- The server runs on a laptop or a small always-on box; remote agents reach it over HTTP. No cloud services are required.
- The project repository is public; the maintainer's vault is private. Nothing vault-specific may leak into code, fixtures or docs.

## Goals / Non-Goals

**Goals:**
- Single static binary, zero external services, `go install`-able.
- Search that feels instant to an LLM client (p95 ≤ 100 ms warm) and stays fresh without manual reindexing.
- Every write is safe: sandboxed path, validated frontmatter, one commit, never a lost update.
- Behavior fully covered by the specs; implementation swappable behind small interfaces (index, git, transport).

**Non-Goals:**
- Replacing Obsidian sync or being a general Git UI.
- Semantic search in this change (kept behind an `Indexer` interface so an embeddings-based index can be added later).
- Multi-tenant auth; one token = full access to one vault.

## Decisions

### D1. Language and MCP SDK: Go + official `modelcontextprotocol/go-sdk`
Go gives a static binary, fast startup for stdio launches, and good concurrency for the watcher/sync loops. The official SDK tracks the MCP spec (tools, resources, streamable HTTP) and avoids hand-rolling JSON-RPC.
*Alternatives:* `mark3labs/mcp-go` (mature, but community-maintained and diverging from spec updates); TypeScript (slower startup, larger runtime footprint on servers).

### D2. Full-text engine: Bleve v2, index stored outside the vault
Bleve is pure Go (no cgo), supports per-field analyzers with Snowball stemming for Russian and English, BM25 scoring, highlighting, and faceting for tags/folders. The index lives in an OS cache dir (configurable) and is rebuilt if its schema version or the vault path changes.
*Alternatives:* SQLite FTS5 via `modernc.org/sqlite` (no stemming for Russian, trigram only); an in-memory hand-rolled inverted index (fast but no ranking/highlighting, more code to own); Meilisearch/Typesense (external service, violates zero-dependency goal).

### D3. Git operations through the `git` CLI, not go-git
Shelling out reuses the user's SSH agent, credential helpers, `includeIf` configs, commit signing and LFS, and behaves exactly like the desktop editor's Git. Wrapped behind a `Repo` interface with a fake for tests.
*Alternatives:* go-git (pure Go, but SSH/credential edge cases and worse performance on large trees); libgit2 bindings (cgo).

### D4. Frontmatter parsing with `gopkg.in/yaml.v3` Node API
Using the Node API instead of `map[string]any` preserves key order, comments and scalar styles on round-trip, so `set_frontmatter` edits do not rewrite the whole header. The body is stored as raw bytes; line endings are detected and preserved.
*Alternatives:* `adrg/frontmatter` (convenient but loses order); custom parser (unnecessary).

### D5. Write path: lock → pull → mutate → commit → index → debounced push
A single vault-level mutex serializes writes. Before mutating, the server fetches and fast-forwards (or rebases its own unpushed commits) so the write lands on the latest remote state. The file write and `git commit` happen inside the lock; the index update is synchronous so the note is searchable when the tool returns; push is asynchronous, debounced and retried. Optimistic concurrency uses a content hash returned by read tools (`etag`) and checked by write tools.

### D6. Freshness from two signals: filesystem watcher + Git HEAD polling
`fsnotify` on the vault (recursive, debounced 500 ms) catches desktop-editor changes; comparing `HEAD` after each pull catches remote changes. Both feed the same incremental indexer. A full rescan runs on startup when the index is empty or its stored vault revision differs from `HEAD`.

### D7. Conflicts are surfaced, never auto-resolved
If a pull cannot fast-forward or rebase cleanly, the server aborts the rebase, keeps local commits, flips to a `conflict` state, rejects writes with `sync_conflict`, and keeps serving reads. A human (or a future tool) resolves it in the clone. Force-push, `reset --hard` and `commit --amend` are never executed.

### D8. Link resolution follows Obsidian rules
Wikilinks resolve by shortest unique path (basename first, then relative path), support `|alias`, `#heading` and `^block` suffixes, and are case-insensitive on case-insensitive filesystems. Move/rename rewrites links in other notes by default (as Obsidian does), producing a single commit with all touched files.

### D9. HTTP transport is opt-in and token-gated
Streamable HTTP binds only when configured. A bearer token is mandatory for non-loopback addresses; comparison is constant-time. TLS is delegated to a reverse proxy or private network (Tailscale/WireGuard); the docs say so explicitly.

### D10. Tool surface (v1)
Read: `kb_get_note`, `kb_get_section`, `kb_list`, `kb_backlinks`, `kb_tags`.
Search: `kb_search`, `kb_query`, `kb_context`, `kb_quick_open`.
Write: `kb_create_note`, `kb_replace_note`, `kb_patch_note`, `kb_move_note`, `kb_delete_note`.
Git: `kb_history`, `kb_show_revision`, `kb_diff`, `kb_restore`, `kb_sync_status`, `kb_sync_now`.
Ops: `kb_info`, `kb_reindex`.
Resources: `kb://note/<path>` (Markdown), `kb://folder/<path>` (listing).

## Risks / Trade-offs

- [Bleve index size and rebuild time on 10k notes] → benchmark early with a generated fixture; store only fields needed for ranking and snippets; rebuild is incremental after first run.
- [Desktop editor writing the same file at the same moment] → write via temp file + atomic rename; detect mtime/hash changes since the `etag` and reject with `conflict`.
- [Uncommitted foreign changes in the clone block a clean pull] → default: stash-free approach, commit only the server's files and warn in `kb_sync_status`; optional `autocommit_external` to commit foreign changes with a distinct message.
- [Push storms from many small edits] → debounce (5 s default) and coalesce; commits remain granular.
- [Exposing personal data over HTTP] → token mandatory, loopback default, read-only mode, documented TLS guidance.
- [Russian stemming quality] → Snowball is adequate for search; exact-phrase and prefix queries remain available as fallbacks.

## Migration Plan

Greenfield. Deployment is a binary plus a config; rollback is stopping the process. The index can be deleted at any time and is rebuilt on start. The vault repository is never modified in a non-additive way, so switching the server off leaves a normal Git clone behind.

## Open Questions

- Should `kb_delete_note` default to soft delete into `.trash/` (Obsidian convention) or hard delete? Draft assumes soft delete.
- Should templates support only Obsidian core placeholders (`{{date}}`, `{{time}}`, `{{title}}`) or also a small expression syntax? Draft assumes core placeholders only.
- Default `pull_interval` (60 s) versus pull-on-demand only; adjustable without spec changes.
