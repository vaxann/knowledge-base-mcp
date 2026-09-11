## Context

See proposal.md for motivation. Constraints that shape the design:

- The vault is an Obsidian-compatible Markdown tree: YAML frontmatter, `[[wikilinks]]`, `#tags`, per-folder conventions documented for humans, and many binary attachments (PDF, images, Office files) next to notes.
- Notes may mix Cyrillic and Latin content, so tokenisation and stemming must handle both.
- Target size: on the order of 1,000–10,000 notes, with attachments that can make the repository hundreds of MB, so the index must never scan binaries.
- Deployment model: each server instance runs next to its MCP client, typically as a container on a different machine from where humans edit the vault. Every instance owns a private clone and nothing else writes to that work tree. The Git remote (GitHub) is the only shared state; humans keep editing in their own clones. Several instances may be alive at once.
- Only the `main` branch is used. No branches, no pull requests, no HTTP API: history on `main` is the audit trail.
- The project repository is public; the maintainer's vault is private. Nothing vault-specific may leak into code, fixtures or docs.

## Goals / Non-Goals

**Goals:**
- Single static binary and a small container image, zero external services.
- Search that feels instant to an LLM client (p95 ≤ 100 ms warm) and stays fresh through pulls without manual reindexing.
- Every write is safe: sandboxed path, validated frontmatter and schema, one commit, never a lost edit even when conflicts are resolved automatically.
- Behavior fully covered by the specs; implementation swappable behind small interfaces (index, git, validator).

**Non-Goals:**
- Replacing Obsidian sync or being a general Git UI.
- Attachment text extraction and semantic search in this change (kept behind the `Indexer` interface; see Roadmap in proposal.md).
- Multi-user authorization; whoever can launch the process has full access to the clone.

## Decisions

### D1. Language and MCP SDK: Go + official `modelcontextprotocol/go-sdk`
Go gives a static binary, fast startup for stdio launches, and good concurrency for the sync loop. The official SDK tracks the MCP spec (tools, resources) and avoids hand-rolling JSON-RPC.
*Alternatives:* `mark3labs/mcp-go` (mature, community-maintained, drifts from spec updates); TypeScript (slower startup, larger image).

### D2. Full-text engine: Bleve v2, index stored outside the vault
Bleve is pure Go (no cgo), supports per-field analyzers with Snowball stemming for Russian and English, BM25 scoring, highlighting, and faceting for tags/folders. The index lives in a cache directory (a second container volume) and is rebuilt if its schema version or recorded vault revision disagrees with `HEAD`. Documents carry a `source` field (`note` today; `attachment`, `embedding` later) so phase-2 sources extend the same index.
*Alternatives:* SQLite FTS5 via `modernc.org/sqlite` (no Russian stemming, trigram only); hand-rolled inverted index (no ranking/highlighting); Meilisearch/Typesense (external service).

### D3. Git operations through the `git` CLI, not go-git
Shelling out reuses SSH agent/keys, credential helpers, `includeIf` configs, signing and LFS, and behaves exactly like the desktop editor's Git. In the container the CLI is installed alongside the binary. Wrapped behind a `Repo` interface with a fake for tests.
*Alternatives:* go-git (pure Go, but SSH/credential edge cases and slow on large trees); libgit2 bindings (cgo).

### D4. Frontmatter parsing with `gopkg.in/yaml.v3` Node API
Using the Node API instead of `map[string]any` preserves key order, comments and scalar styles on round-trip, so `set_frontmatter` edits do not rewrite the whole header. Serialisation always quotes ambiguous scalars (values with `: `, `#`, leading `*`/`&`, `yes/no/on/off`, numeric-looking strings) so Dataview keeps parsing the card. The body is stored as raw bytes; line endings are detected and preserved.
*Alternatives:* `adrg/frontmatter` (loses order); custom parser (unnecessary).

### D5. Write path: lock → pull (if stale) → validate → mutate → commit → index → debounced push
A single vault-level mutex serializes writes and sync. Before mutating, the server integrates the remote unless a pull succeeded within the freshness window, so the write lands on the latest remote state. Validation (YAML + schema) runs on the would-be result before anything touches disk. File write and `git commit` happen inside the lock; the index update is synchronous so the note is searchable when the tool returns; push is asynchronous, debounced and retried. Optimistic concurrency uses a content hash (`etag`) returned by read tools and checked by write tools.

### D6. Freshness comes only from the server's own writes and from pulls
Because the clone is private to the instance, there is no filesystem watcher. After every pull the server diffs the previous and new `HEAD` (`git diff --name-status`) and upserts/deletes exactly those documents. A full rescan runs on startup when the index is empty or its recorded revision differs from `HEAD`.
*Alternative:* fsnotify (rejected: unnecessary complexity for a clone nobody else touches; unreliable in containers and on network volumes).

### D7. Conflicts are resolved automatically, losing side becomes a sibling note
Integration is `git pull --rebase`. When a file conflicts while replaying a server commit, the resolver applies the configured strategy per file: with `remote-wins` (default) it checks out the upstream side, extracts the replayed commit's version (`git show <commit>:<path>`) and writes it as `<stem>.conflict-<short hash>.md` next to the note, stages both and continues the rebase. Delete/modify conflicts are handled the same way. The result is linear history with an explicit, searchable record of what was overridden; a client can merge it later with `kb_patch_note` and delete the sibling. Human edits win by default because a human is not going to re-read the vault for a lost change, an agent can.
*Alternatives:* stop and wait for a human (rejected by the maintainer: instances are unattended); `local-wins` (available as an option); three-way merge via an LLM inside the server (out of scope, no LLM in the server).

### D8. Link resolution follows Obsidian rules; the server never rewrites links
Wikilinks resolve by shortest unique path (basename first, then relative path), support `|alias`, `#heading` and `^block` suffixes, and are case-insensitive on case-insensitive filesystems. Resolution is used for backlinks and for reporting which notes reference a moved note. Rewriting links is left to the client: it can `kb_grep` for the old name and `kb_patch_note` each file, keeping every edit explicit in history.
*Alternative:* Obsidian-style automatic rewriting (rejected: hidden multi-file edits, ambiguity when several notes share a basename).

### D9. Grep is a second, index-free search path
`kb_grep` walks visible notes concurrently and matches literally or with RE2 (`regexp`), skipping binaries by extension and size. For vaults of a few thousand notes a parallel scan is well under the latency budget, so no trigram index is needed. In phase 2 the same walk will include cached extracted text of attachments.
*Alternative:* shelling out to `ripgrep` (rejected: extra host dependency; Go's `regexp` is fast enough at this scale).

### D10. Deletion is permanent; Git is the trash
`kb_delete_note` removes the file and commits. Recovery is `kb_ls_tree` / `kb_show_revision` / `kb_restore` over Git history, which also covers notes deleted or renamed by other clones. Following renames in `kb_history` uses `git log --follow`.
*Alternative:* Obsidian-style `.trash/` (rejected: duplicates Git and leaks deleted content into listings and search).

### D11. No templates in the server
Templates are ordinary notes in a folder the user chooses; a client reads one with `kb_get_note` and passes the filled content to `kb_create_note`.

### D12. Schemas live in the vault as `_schema.yaml`
Folder conventions already live next to the notes (human-readable README files). A machine-readable `_schema.yaml` beside them keeps validation rules versioned with the data, editable through the same tools, and identical for every instance. The format is a small JSON-Schema-like subset (required, typed properties, enum, pattern, filename pattern) implemented directly rather than pulling a full JSON Schema library. Schema files are cached in memory and invalidated when a write or pull touches them.
*Alternatives:* schemas in server config (rejected: drift between instances and vault); full JSON Schema (rejected: heavier than needed, poor error messages for LLM consumers).

### D13. stdio only, packaged as a container
No HTTP listener: every client launches its own instance (`docker run -i ghcr.io/…` or the binary) and the remote is the rendezvous point. The image is a distroless/alpine base with `git` and `openssh-client`; volumes for the clone and the index; credentials via a read-only mounted SSH key (`GIT_SSH_COMMAND` set by the entrypoint) or an HTTPS token consumed by a Git credential helper configured at start. Removing HTTP removes the whole auth surface from the server.
*Alternative:* streamable HTTP with bearer tokens (rejected by the maintainer as unnecessary; can be revisited as a separate change).

### D14. Tool surface (v1)
Read: `kb_get_note`, `kb_get_section`, `kb_list`, `kb_backlinks`, `kb_tags`.
Search: `kb_search`, `kb_grep`, `kb_query`, `kb_context`, `kb_quick_open`.
Write: `kb_create_note`, `kb_replace_note`, `kb_patch_note`, `kb_move_note`, `kb_delete_note`.
Validation: `kb_validate`.
Git: `kb_log`, `kb_history`, `kb_ls_tree`, `kb_show_revision`, `kb_diff`, `kb_restore`, `kb_sync_status`, `kb_sync_now`.
Ops: `kb_info`, `kb_reindex`.
Resources: `kb://note/<path>` (Markdown), `kb://folder/<path>` (listing).

## Risks / Trade-offs

- [Bleve index size and rebuild time on 10k notes] → benchmark early with a generated fixture; index only fields needed for ranking and snippets; rebuild is incremental after first run.
- [Automatic resolution hides a bad merge] → the losing version is always preserved as a sibling note and reported in `kb_sync_status`; tool descriptions tell clients to check `last_conflicts`.
- [Two instances editing the same note within seconds] → pull-before-write with a short freshness window shrinks the race; the rare loser is preserved as a conflict note.
- [Sibling conflict notes accumulate] → `kb_validate` and `kb_query` can list them (`path` glob `*.conflict-*`); a cleanup task is a client decision.
- [Push storms from many small edits] → debounce (5 s default) and coalesce; commits remain granular.
- [Credentials inside a container] → key mounted read-only, never copied, never logged; documented threat model.
- [Russian stemming quality] → Snowball is adequate for ranked search; `kb_grep` gives exact matching when stemming gets in the way.
- [Client forgets to fix links after a rename] → `kb_move_note` returns `referencing_notes`; tool description tells the model to review them.
- [Schema too strict blocks useful writes] → `warn` mode and folder-scoped schemas; schemas are editable through the server itself.

## Migration Plan

Greenfield. Deployment is an image plus environment variables; rollback is stopping the container. The index can be deleted at any time and is rebuilt on start. The vault repository is never modified in a non-additive way, so switching the server off leaves a normal Git clone behind. Existing prose conventions can be turned into `_schema.yaml` files gradually; folders without a schema get YAML-only validation.

## Open Questions

- Default `pull_interval` (60 s) and freshness window (5 s) for unattended instances; adjustable without spec changes.
