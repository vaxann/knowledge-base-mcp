## Why

AI assistants (Claude, ChatGPT, local models, autonomous agents) need fast, reliable and versioned access to a personal knowledge base that lives as an Obsidian-compatible Markdown vault in a Git repository. Today they can only read raw files or rely on ad-hoc scripts: search is slow and naive, edits are unversioned, and nothing enforces vault conventions (YAML frontmatter, wikilinks, folder rules). A single MCP server that any client can talk to, locally or remotely, removes that friction.

## What Changes

- New Go MCP server (`knowledge-base-mcp`) exposing one vault through MCP tools and resources over **stdio** and **streamable HTTP** (bearer-token protected).
- **Fast search** in three forms, all on by default: a ranked full-text index (Cyrillic and Latin, stemming, frontmatter filters) with sub-100 ms queries kept up to date incrementally; grep-style exact/regex matching across notes; and Dataview-style metadata queries. A "context bundle" tool returns the best-matching sections within a size budget for retrieval-augmented answers.
- **Read tools**: get a note with parsed frontmatter, sections, tags, outgoing links and backlinks; list folders; Dataview-style metadata queries.
- **Write tools**: create, replace, patch (append, replace section, set frontmatter), move/rename, permanent delete, all with optimistic concurrency. No templates and no automatic link rewriting: the server is a data-access layer, the calling model decides.
- **Git versioning**: every mutation is exactly one atomic commit; automatic pull and debounced push; tools to walk the commit log, list the tree at any revision, read and diff past versions (including deleted notes) and restore them as new commits; conflicts never resolved by force.
- **Vault safety**: vault-relative paths only, deny-listed folders, non-Markdown files never edited, read-only mode, structured logs without note bodies or secrets.
- Project scaffolding: CI (build, test, lint, vulnerability check), release pipeline, synthetic fixture vault for tests.

## Capabilities

### New Capabilities
- `vault-access`: configuration, vault discovery, path sandboxing, exclusion rules, note/frontmatter model.
- `note-read`: reading notes, sections, listing, tags, links and backlinks, MCP resources.
- `note-write`: creating, replacing, patching, moving and deleting notes with validation and concurrency control.
- `search`: full-text search, grep-style search, filters, metadata queries, context bundles, index lifecycle and latency targets.
- `git-versioning`: commit-per-mutation, auto pull/push, conflict state, history navigation, diff and restore, sync status.
- `mcp-transport`: MCP protocol surface, stdio and HTTP transports, authentication, concurrency, logging, health.

### Modified Capabilities
<!-- none: this is the first change in the project -->

## Impact

- New codebase: `cmd/knowledge-base-mcp`, `internal/{config,vault,search,gitsync,mcp}`.
- External dependencies: the official MCP Go SDK, a pure-Go full-text engine (Bleve), YAML parser, filesystem watcher; the `git` CLI must be installed on the host.
- Runtime footprint: a local Git clone of the vault plus an on-disk search index outside the vault.
- Security surface: the HTTP transport exposes personal data and therefore requires a token and is expected to sit behind TLS (reverse proxy or private network).
- Non-goals for this change: semantic/vector search, multiple vaults per server, multi-user authorization, editing binary attachments, evaluating Dataview queries embedded in notes, note templates, automatic link rewriting on rename, a trash folder (Git history is the undo mechanism), and any in-server LLM (the calling model answers using search results).
