## Why

AI assistants (Claude, ChatGPT, local models, autonomous agents) need fast, reliable and versioned access to a personal knowledge base that lives as an Obsidian-compatible Markdown vault in a Git repository. Today they can only read raw files or rely on ad-hoc scripts: search is slow and naive, edits are unversioned, and nothing enforces vault conventions (YAML frontmatter, wikilinks, folder rules). A single MCP server that any client can talk to, locally or remotely, removes that friction.

## What Changes

- New Go MCP server (`knowledge-base-mcp`) exposing one vault through MCP tools and resources over **stdio**, shipped as a binary and a container image. Each client runs its own instance with a private clone; the Git remote is the only shared state.
- **Fast search** in three forms, all on by default: a ranked full-text index (Cyrillic and Latin, stemming, frontmatter filters) with sub-100 ms queries kept up to date incrementally; grep-style exact/regex matching across notes; and Dataview-style metadata queries. A "context bundle" tool returns the best-matching sections within a size budget for retrieval-augmented answers.
- **Read tools**: get a note with parsed frontmatter, sections, tags, outgoing links and backlinks; list folders; Dataview-style metadata queries.
- **Write tools**: create, replace, patch (append, replace section, set frontmatter), move/rename, permanent delete, all with optimistic concurrency. No templates and no automatic link rewriting: the server is a data-access layer, the calling model decides.
- **Git versioning**: every mutation is exactly one atomic commit under a dedicated author; automatic pull (rebase) and debounced push on `main` only; conflicts resolved automatically with the losing version preserved as a sibling note; tools to walk the commit log, list the tree at any revision, read and diff past versions (including deleted notes) and restore them as new commits.
- **Card validation**: frontmatter must be valid YAML on every write, with server-side quoting of ambiguous values; optional per-folder `_schema.yaml` (required fields, types, enums, file-name pattern) enforced in strict or warn mode; `kb_validate` audits existing notes.
- **Vault safety**: vault-relative paths only, deny-listed folders, non-Markdown files never edited, read-only mode, structured logs without note bodies or secrets.
- Project scaffolding: CI (build, test, lint, vulnerability check), release pipeline, synthetic fixture vault for tests.

## Capabilities

### New Capabilities
- `vault-access`: configuration, vault discovery, path sandboxing, exclusion rules, note/frontmatter model.
- `note-read`: reading notes, sections, listing, tags, links and backlinks, MCP resources.
- `note-write`: creating, replacing, patching, moving and deleting notes with validation and concurrency control.
- `search`: full-text search, grep-style search, filters, metadata queries, context bundles, index lifecycle and latency targets.
- `git-versioning`: commit-per-mutation, auto pull/push on `main`, automatic conflict resolution, history navigation, diff and restore, sync status.
- `schema-validation`: YAML validity, in-vault folder schemas, strict/warn enforcement, vault audit.
- `mcp-transport`: MCP protocol surface, stdio transport, container deployment, concurrency, logging, operational tools.

### Modified Capabilities
<!-- none: this is the first change in the project -->

## Impact

- New codebase: `cmd/knowledge-base-mcp`, `internal/{config,vault,search,gitsync,mcp}`.
- External dependencies: the official MCP Go SDK, a pure-Go full-text engine (Bleve), YAML parser; the `git` CLI must be installed on the host or in the image.
- Runtime footprint: a private Git clone of the vault plus an on-disk search index, both as container volumes.
- Security surface: no network listener; the only secret is the Git credential mounted into the container.
- Non-goals for this change: multiple vaults per server, multi-user authorization, an HTTP transport, editing binary attachments, evaluating Dataview queries embedded in notes, note templates, automatic link rewriting on rename, a trash folder (Git history is the undo mechanism), and any in-server LLM (the calling model answers using search results).

## Roadmap (separate changes after this one)

- **Attachment text extraction**: index the text of PDF, DOCX, XLSX and similar files (with OCR for scanned pages to be decided) so `kb_search`, `kb_grep` and `kb_context` also find content inside documents.
- **Semantic search**: embeddings-based retrieval alongside the full-text index (`kb_search` hybrid ranking, `kb_context` reranking); embedding provider to be decided.
