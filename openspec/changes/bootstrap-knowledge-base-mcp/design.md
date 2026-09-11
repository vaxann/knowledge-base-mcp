## Context

See proposal.md for motivation. Constraints that shape the design:

- The vault is a plain Markdown tree as produced by Obsidian, Logseq or by hand: optional YAML frontmatter, `[[wikilinks]]`, `#tags`, and many binary attachments (PDF, images, Office files) next to notes. The server understands these conventions but does not depend on any editor and imposes no structure on content.
- Notes may mix Cyrillic and Latin content, so tokenisation and stemming must handle both.
- Target size: on the order of 1,000–10,000 notes, with attachments that can make the repository hundreds of MB, so the index must never scan binaries.
- Deployment model: each server instance runs next to its MCP client, typically as a container on a different machine from where humans edit the vault. Every instance owns a private clone and nothing else writes to that work tree. The Git remote (GitHub) is the only shared state; humans keep editing in their own clones. Several instances may be alive at once.
- Only the `main` branch is used. No branches, no pull requests: history on `main` is the audit trail.
- The project repository is public; the maintainer's vault is private. Nothing vault-specific may leak into code, fixtures or docs.

## Goals / Non-Goals

**Goals:**
- Single static binary and a small container image, zero external services.
- Search that feels instant to an LLM client (p95 ≤ 100 ms warm) and stays fresh through pulls without manual reindexing.
- Every write is safe: sandboxed path, one commit, never a lost edit; conflicts are made visible instead of being decided by the server.
- Behavior fully covered by the specs; implementation swappable behind small interfaces (index, git).

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
Using the Node API instead of `map[string]any` preserves key order, comments and scalar styles on round-trip, so `set_frontmatter` edits do not rewrite the whole header. Serialisation always quotes ambiguous scalars (values with `: `, `#`, leading `*`/`&`, `yes/no/on/off`, numeric-looking strings) so editors keep parsing the header. Unparseable frontmatter is tolerated on read and on whole-file writes (reported as a warning) and only blocks frontmatter-level patch operations. The body is stored as raw bytes; line endings are detected and preserved.
*Alternatives:* `adrg/frontmatter` (loses order); custom parser (unnecessary).

### D5. Write path: lock → pull (if stale) → mutate → commit → index → debounced push
A single vault-level mutex serializes writes and sync. Before mutating, the server integrates the remote unless a pull succeeded within the freshness window, so the write lands on the latest remote state. File write and `git commit` happen inside the lock; the index update is synchronous so the note is searchable when the tool returns; push is asynchronous, debounced and retried. Optimistic concurrency uses a content hash (`etag`) returned by read tools and checked by write tools.

### D6. Freshness comes only from the server's own writes and from pulls
Because the clone is private to the instance, there is no filesystem watcher. After every pull the server diffs the previous and new `HEAD` (`git diff --name-status`) and upserts/deletes exactly those documents. A full rescan runs on startup when the index is empty or its recorded revision differs from `HEAD`.
*Alternative:* fsnotify (rejected: unnecessary complexity for a clone nobody else touches; unreliable in containers and on network volumes).

### D7. A conflicting merge is frozen locally and handed to the client
Integration is `git pull --no-rebase`. When Git reports conflicts, the server leaves the merge in progress exactly as Git does (`MERGE_HEAD`, index stages 1/2/3, markers in the work tree), flips to `conflict`, and refuses every write with `merge_conflict` carrying `base`/`ours`/`theirs` (`git show :1:`, `:2:`, `:3:`) for each path. Nothing is committed or pushed, so humans never see markers in their editors and history stays clean. The model calling the server has far more context than any heuristic, so it produces the resolution; `kb_resolve_conflict` stages it, commits the merge with a `KB-Client` trailer, re-indexes and pushes. Because the state is plain Git state on disk, a restart resumes in `conflict` rather than losing anything. Reads keep working so the model can gather context (`kb_history`, `kb_diff`, neighbours) before resolving.
*Alternatives:* commit the merge with markers and push (rejected: markers leak to every clone and editor); a server-side winner (`remote-wins`) with a sibling conflict note (rejected: the server would decide); rebase (rejected: per-commit conflict loops are harder to present to a client than one merge).

### D8. Link resolution follows common wikilink rules; the server never rewrites links
Wikilinks resolve by shortest unique path (basename first, then relative path), support `|alias`, `#heading` and `^block` suffixes, and are case-insensitive on case-insensitive filesystems. Resolution is used for backlinks and for reporting which notes reference a moved note. Rewriting links is left to the client: it can `kb_grep` for the old name and `kb_patch_note` each file, keeping every edit explicit in history.
*Alternative:* editor-style automatic rewriting (rejected: hidden multi-file edits, ambiguity when several notes share a basename).

### D9. Grep is a second, index-free search path
`kb_grep` walks visible notes concurrently and matches literally or with RE2 (`regexp`), skipping binaries by extension and size. For vaults of a few thousand notes a parallel scan is well under the latency budget, so no trigram index is needed. In phase 2 the same walk will include cached extracted text of attachments.
*Alternative:* shelling out to `ripgrep` (rejected: extra host dependency; Go's `regexp` is fast enough at this scale).

### D10. Deletion is permanent; Git is the trash
`kb_delete_note` removes the file and commits. Recovery is `kb_ls_tree` / `kb_show_revision` / `kb_restore` over Git history, which also covers notes deleted or renamed by other clones. Following renames in `kb_history` uses `git log --follow`.
*Alternative:* an editor-style `.trash/` folder (rejected: duplicates Git and leaks deleted content into listings and search).

### D11. No templates in the server
Templates are ordinary notes in a folder the user chooses; a client reads one with `kb_get_note` and passes the filled content to `kb_create_note`.

### D12. stdio per client, or one token-protected HTTP instance; packaged as a multi-arch container
Desktop clients launch the binary or `docker run -i` and talk stdio. For a permanently running instance that several agents (possibly on other machines) share, the same binary serves the SDK's streamable HTTP transport at `/mcp` when `KB_HTTP_LISTEN` is set. Access control is a single bearer token compared in constant time; the server refuses to bind a non-loopback address without one, so an exposed instance is never unauthenticated by accident. TLS is either terminated by a reverse proxy/private network or served directly from a configured cert/key. `docker-compose.yml` wires the long-running mode with health checks and restarts. An exclusive lock on the index directory guarantees one process per clone, which is what keeps the in-process vault lock sufficient.
*Alternatives:* a container-internal Unix socket with `docker exec` attach (implemented and dropped: cannot reach other machines, and its only access control is filesystem permissions); per-client tokens with revocation (deferred: one token per instance is enough for a personal vault; rotate by restarting with a new `KB_HTTP_TOKEN`).
The image is an alpine base with `git` and `openssh-client`, built for `linux/amd64` and `linux/arm64`; volumes for the clone and the index; credentials via a read-only mounted SSH key (copied by a root entrypoint that then drops to an unprivileged user) or an HTTPS token consumed by a Git credential helper. Both paths are first-class and covered by tests.

### D13. Tool surface (v1)
Read: `kb_get_note`, `kb_get_section`, `kb_list`, `kb_backlinks`, `kb_tags`.
Search: `kb_search`, `kb_grep`, `kb_query`, `kb_context`, `kb_quick_open`.
Write: `kb_create_note`, `kb_replace_note`, `kb_patch_note`, `kb_move_note`, `kb_delete_note`.
Git: `kb_log`, `kb_history`, `kb_ls_tree`, `kb_show_revision`, `kb_diff`, `kb_restore`, `kb_sync_status`, `kb_sync_now`, `kb_conflicts`, `kb_resolve_conflict`.
Ops: `kb_info`, `kb_reindex`.
Resources: `kb://note/<path>` (Markdown), `kb://folder/<path>` (listing).

## Risks / Trade-offs

- [Bleve index size and rebuild time on 10k notes] → benchmark early with a generated fixture; index only fields needed for ranking and snippets; rebuild is incremental after first run.
- [An unattended instance sits in `conflict` for a long time, its own commits unpushed] → reads still work; the next client that writes is forced to resolve; `kb_sync_status` makes the backlog visible; other instances are unaffected because nothing was pushed.
- [Two instances editing the same note within seconds] → pull-before-write with a short freshness window shrinks the race; a real collision becomes a frozen merge handed to a client, never a lost edit.
- [Client resolves badly] → the resolution is one reviewable merge commit with a `KB-Client` trailer; `kb_restore` and `kb_show_revision` recover any side from history.
- [Push storms from many small edits] → debounce (5 s default) and coalesce; commits remain granular.
- [Credentials inside a container] → key mounted read-only and copied only into the container's private home, never logged; documented threat model.
- [HTTP token leaks or is brute-forced] → constant-time compare, 401 without side effects, token only in env, loopback binding by default in Compose, rotation by restart; TLS guidance in docs.
- [Russian stemming quality] → Snowball is adequate for ranked search; `kb_grep` gives exact matching when stemming gets in the way.
- [Client forgets to fix links after a rename] → `kb_move_note` returns `referencing_notes`; tool description tells the model to review them.

## Migration Plan

Greenfield. Deployment is an image plus environment variables; rollback is stopping the container. The index can be deleted at any time and is rebuilt on start. The vault repository is never modified in a non-additive way, so switching the server off leaves a normal Git clone behind.

## Open Questions

- Default `pull_interval` (60 s) and freshness window (5 s) for unattended instances; adjustable without spec changes.
