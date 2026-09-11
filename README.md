# Knowledge Base MCP

An [MCP](https://modelcontextprotocol.io) server, written in Go, that gives AI assistants fast, versioned access to a personal knowledge base kept as a folder of Markdown files inside a Git repository. Works with an Obsidian or Logseq vault or any plain Markdown tree; the server understands frontmatter, wikilinks and tags but imposes no structure on your content.

> **Status:** first implementation complete and covered by tests; not yet released. The requirements live in [`openspec/`](openspec/).

## What it does

- **Fast search, three ways** — ranked full-text search with Cyrillic and Latin stemming and frontmatter filters; grep-style exact or regex matching across all notes; Dataview-style metadata queries. A "context bundle" tool returns the most relevant sections within a size budget, so any LLM can answer questions about the vault with a single call.
- **Read notes** — get a note with parsed YAML frontmatter, headings, tags, outgoing links and backlinks; list folders; query notes by frontmatter fields (Dataview‑style).
- **Edit notes** — create, replace or patch content (append, replace a section, set frontmatter keys), rename and delete, with optimistic concurrency. The server is a data-access layer: no templates, no magic link rewriting, the calling model stays in control.
- **Versioning for free** — every change is an atomic Git commit under a dedicated author on `main`; the server pulls and pushes automatically and lets clients walk the log, list the tree at any revision, read and diff old versions (deleted notes included) and restore them. History is never rewritten.
- **Runs anywhere your agent runs** — stdio only, as a binary or a multi-arch container (amd64, arm64). Every client gets its own instance with a private clone; the Git remote is the only shared state. When a merge conflicts, the server does not guess: it freezes the merge locally, refuses writes with the full conflict (base, ours, theirs), and lets the agent, which has the context, submit the resolution. Nothing is pushed until then and nothing is ever lost.
- **Safe by default** — vault‑relative paths only, deny‑listed folders (`.obsidian/`, `.git/`, …), read‑only mode, no note content or secrets in logs.

## Architecture at a glance

```
MCP client (Claude, Cursor, custom agent)
        │  stdio
        ▼
┌───────────────────────────────┐
│  knowledge-base-mcp (Go)      │   (binary or container)
│  ├─ tools: kb_search, kb_get… │
│  ├─ vault: frontmatter, links │
│  ├─ index: full-text (Bleve)  │
│  └─ git: commit / pull / push │
└───────────────┬───────────────┘
                ▼
   private clone of your vault  ⇄  Git remote  ⇄  your editor's clone
```

## Quick start

```bash
go install github.com/vaxann/knowledge-base-mcp/cmd/knowledge-base-mcp@latest
export KB_VAULT_PATH=$HOME/.local/share/kb/vault      # private clone, created on first start
export KB_GIT_REMOTE=git@github.com:me/my-notes.git    # your vault repository
knowledge-base-mcp -check                              # validates config, clones, builds the index
```

Then register `knowledge-base-mcp` as an MCP command in your client. See [docs/clients.md](docs/clients.md) for Claude Desktop, Claude Code, Cursor and container setups (SSH key or HTTPS token).

## Tools

| Group | Tools |
|---|---|
| Read | `kb_get_note`, `kb_get_section`, `kb_list`, `kb_backlinks`, `kb_tags` |
| Search | `kb_search`, `kb_grep`, `kb_query`, `kb_context`, `kb_quick_open` |
| Write | `kb_create_note`, `kb_replace_note`, `kb_patch_note`, `kb_move_note`, `kb_delete_note`, `kb_restore`, `kb_resolve_conflict` |
| History & sync | `kb_log`, `kb_history`, `kb_ls_tree`, `kb_show_revision`, `kb_diff`, `kb_sync_status`, `kb_sync_now`, `kb_conflicts` |
| Ops | `kb_info`, `kb_reindex` |

Resources: `kb://note/<path>` (Markdown) and `kb://folder/<path>` (JSON listing).

## Configuration

| Env var           | Purpose                                              |
|-------------------|------------------------------------------------------|
| `KB_VAULT_PATH`   | Local Git clone of the vault (required)              |
| `KB_GIT_REMOTE`   | Remote URL used to bootstrap‑clone if path is empty  |
| `KB_GIT_TOKEN`    | HTTPS token handed to Git (alternative to an SSH key)|
| `KB_INDEX_DIR`    | Where the search index is stored                     |
| `KB_READ_ONLY`    | Disable all write tools                              |

See [docs/configuration.md](docs/configuration.md) for the full reference and error codes, and [docs/conflicts.md](docs/conflicts.md) for how frozen merges are handed to the client. Real configuration files and credentials are git‑ignored and must never be committed.

## Roadmap

After the first release: text extraction from PDF and Office attachments so search covers documents too, and semantic (embedding-based) search next to full text.

## Development

Requirements: Go 1.26+, `git`, and the [OpenSpec](https://github.com/Fission-AI/OpenSpec) CLI for spec‑driven changes.

```bash
make build      # bin/knowledge-base-mcp
make race       # go test -race ./...
make lint       # golangci-lint
make bench      # 5,000-note search benchmark (see docs/benchmarks.md)
openspec list   # active change proposals
```

Workflow: every feature starts as an OpenSpec change (`openspec/changes/<name>/`) with a proposal, delta specs, design and tasks; once implemented it is archived into `openspec/specs/`.

## Privacy

This project is public. It must never contain personal notes, real vault paths, repository names of private vaults, tokens or keys. Tests use a synthetic fixture vault under `testdata/`.

## License

[MIT](LICENSE)
