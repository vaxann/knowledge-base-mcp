## 1. Project scaffolding

- [x] 1.1 Create `cmd/knowledge-base-mcp` and `internal/{config,vault,search,gitsync,mcp}` packages with a `main` that prints version; verify `go build ./...` succeeds
- [x] 1.2 Add Makefile targets (`build`, `test`, `lint`, `bench`) and `golangci-lint` config; verify `make lint` passes on the empty skeleton
- [x] 1.3 Add GitHub Actions workflow running build, test, lint and `govulncheck` on push/PR; verify a green run on the default branch
- [x] 1.4 Create a synthetic fixture vault under `testdata/vault` (frontmatter, wikilinks, tags, an excluded folder, a binary attachment, a deleted-then-restored note in history) and a helper that initialises it as a temporary Git repo; verify a test can open it

## 2. Configuration and vault access

- [x] 2.1 Implement config loading (defaults → YAML file → `KB_*` env) with validation; verify unit tests cover precedence and missing-path errors
- [x] 2.2 Implement vault opening: path must be a Git work tree on the configured branch, optional bootstrap clone from remote; verify tests for valid, missing, non-git, wrong-branch and clone cases
- [x] 2.3 Implement path sandboxing (relative only, no `..`, no symlink escape) and exclusion globs with built-in defaults; verify table tests reject traversal and hide excluded paths
- [x] 2.4 Implement the note model: frontmatter parse/serialise with order preservation, body split, line-ending detection; verify round-trip tests are byte-identical

## 3. Note parsing

- [x] 3.1 Implement wikilink and Markdown link extraction with alias/heading/block suffixes; verify tests against fixture notes
- [x] 3.2 Implement Obsidian-style link resolution (shortest unique path, case handling) and a backlink graph; verify tests for ambiguous basenames
- [x] 3.3 Implement tag extraction from frontmatter and inline `#tags`, heading/section splitting; verify tests

## 4. Read tools

- [x] 4.1 Implement `kb_get_note`, `kb_get_section`, `kb_list` (pagination, glob), `kb_backlinks`, `kb_tags`; verify integration tests over the fixture vault
- [x] 4.2 Register `kb://note/<path>` and `kb://folder/<path>` resources; verify an MCP client can read them

## 5. Search index

- [x] 5.1 Define the `Indexer` interface and implement it with Bleve (fields: path, title, body, tags, folder, frontmatter, mtime; ru/en analyzers); verify a Cyrillic stemming test passes
- [x] 5.2 Implement full scan, incremental upsert/delete, on-disk persistence with schema/vault-revision stamps; verify reindex-on-mismatch test
- [x] 5.3 Wire freshness: after each pull, diff old and new HEAD and upsert/delete exactly those documents; verify a test that a pulled commit is searchable when the pull returns
- [x] 5.4 Implement `kb_search` (ranking, title boost, highlights, filters), `kb_quick_open`, `kb_query` (frontmatter predicates, sort, select), `kb_context` (heading-chunked bundle within a budget); verify integration tests
- [x] 5.6 Implement `kb_grep` (parallel scan, literal and RE2, glob, context lines, binary skip); verify tests including matches inside code blocks
- [x] 5.5 Add a benchmark over a generated 5,000-note vault; verify p95 ≤ 100 ms for `kb_search` and ≤ 200 ms for `kb_grep` and record results in `docs/benchmarks.md`

## 6. Write tools

- [x] 6.1 Implement atomic file writes (temp + rename), `etag` computation and conflict detection; verify concurrent-write tests
- [x] 6.2 Implement `kb_create_note` (frontmatter serialisation, parent dirs, exists check) and `kb_replace_note`; verify tests
- [x] 6.3 Implement `kb_patch_note` operations (append, prepend, replace_section, insert_after_heading, set_frontmatter, remove_frontmatter, find_replace) applied atomically, with ambiguous-scalar quoting and a `warnings` entry for unparseable frontmatter on whole-file writes; verify tests including frontmatter-only edits leaving the body untouched
- [x] 6.4 Implement `kb_move_note` (git rename, `referencing_notes` from the backlink graph) and `kb_delete_note` (permanent, etag-checked); verify tests
- [x] 6.5 Enforce read-only mode (write tools not listed); verify a test that `tools/list` omits them

- [x] 6.6 Implement attachments: `kb_get_file` (embedded blob, size cap), `kb_upload_file` (base64, one commit), `kb://file/` resource, move/delete on attachments, HMAC-signed download and upload links with `/files/` and `/upload/` routes (page with multipart, PUT with token), nosniff/CSP; verify tool, resource and HTTP tests including tampering, expiry and folder escape

## 7. Git versioning

- [x] 7.1 Implement the `Repo` interface over the `git` CLI (status, add, commit, fetch, ff/rebase, push, log with --follow, ls-tree, show, diff, rev-parse) plus a fake for tests; verify unit tests against a temp repo
- [x] 7.2 Implement commit-per-mutation with the message format and author config; verify each write tool produces exactly one commit containing only touched files
- [x] 7.3 Implement pull-before-write with freshness window, periodic pull (merge), debounced/retried async push, branch guard; verify tests with a bare remote and a competing clone
- [x] 7.4 Implement the frozen-merge `conflict` state: detect conflicts, keep Git's merge state, block writes with `merge_conflict` carrying base/ours/theirs per path (content, modify/delete, add/add), expose `kb_conflicts`, survive restart; verify tests with a bare remote and a competing clone
- [x] 7.5 Implement `kb_resolve_conflict` (content / take / delete, marker check, partial resolutions, merge commit with trailers, re-index, push, leave state) and current content in stale-`etag` `conflict` errors; verify an end-to-end test where a client resolves and the remote receives one merge commit
- [x] 7.6 Implement `kb_log`, `kb_history`, `kb_ls_tree`, `kb_show_revision`, `kb_diff`, `kb_restore` (including deleted notes), `kb_sync_status`, `kb_sync_now`; verify tests

## 8. Transport and server

- [x] 8.1 Wire all tools with JSON schemas, English descriptions and stable error codes into the MCP server; verify `tools/list` snapshot test
- [x] 8.2 Implement stdio transport (logs to stderr only); verify with a smoke test using an MCP client library
- [x] 8.3 Verify no listening sockets are opened in stdio mode (test inspects the process)
- [x] 8.6 Implement streamable HTTP transport at /mcp with bearer auth (constant-time compare, refuse non-loopback without token), /healthz, optional direct TLS, and the index-directory instance lock; verify 401/200 tests, concurrent-client test and second-instance refusal
- [x] 8.4 Implement the vault write mutex and concurrent reads; verify a race test (`go test -race`) with parallel clients
- [x] 8.5 Implement structured logging with redaction of credentials and note bodies; verify a test that log output never contains a note body

- [x] 8.7 Embed an OAuth 2.1 authorization server (RFC 8414/9728 metadata, RFC 7591 registration, PKCE code flow with password sign-in page, refresh rotation, persisted hashed state) accepted by /mcp alongside the static token; verify a full-flow test including wrong password, single-use code, rotation and restart

## 9. Documentation and release

- [x] 9.1 Write `docs/` pages: configuration reference, client setup (Claude Desktop, Claude Code, Cursor) for binary and container, both credential methods, the conflict state and how a client resolves it; verify links render on GitHub
- [x] 9.2 Add Dockerfile (binary + git + openssh-client, entrypoint wiring both `GIT_SSH_COMMAND` and an HTTPS credential helper, clone-on-empty-volume) and goreleaser config for binaries and a multi-arch (`linux/amd64`, `linux/arm64`) GHCR image; verify `docker run -i` answers `initialize` with each credential method
- [x] 9.4 Add docker-compose.yml and .env.example for a long-running token-protected instance with health check; verify `docker compose up` answers `initialize` with the token and 401 without it
- [x] 9.3 End-to-end scenario test: create → search → grep → patch → move → delete → ls_tree → restore over a fixture remote; verify it passes in CI
