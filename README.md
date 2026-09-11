# Knowledge Base MCP

An [MCP](https://modelcontextprotocol.io) server, written in Go, that gives AI assistants fast, versioned access to a personal knowledge base kept as an Obsidian‑compatible Markdown vault inside a Git repository.

> **Status:** design phase. The requirements live in [`openspec/`](openspec/) and are being refined before implementation starts. Nothing is runnable yet.

## What it will do

- **Fast search** — full-text search over notes with Cyrillic and Latin stemming, frontmatter filters, and a "context bundle" tool that returns the most relevant sections within a token budget, so any LLM can answer questions about the vault with a single call.
- **Read notes** — get a note with parsed YAML frontmatter, headings, tags, outgoing links and backlinks; list folders; query notes by frontmatter fields (Dataview‑style).
- **Edit notes** — create notes (optionally from templates), replace or patch content (append, replace a section, set frontmatter keys), move and delete, with optimistic concurrency.
- **Versioning for free** — every change is an atomic Git commit; the server pulls and pushes automatically, exposes per‑note history, diffs and restore, and never rewrites history.
- **Two transports** — stdio for local clients (Claude Desktop, Claude Code, Cursor, …) and streamable HTTP with bearer‑token auth so remote agents can use the same vault.
- **Safe by default** — vault‑relative paths only, deny‑listed folders (`.obsidian/`, `.git/`, …), read‑only mode, no note content or secrets in logs.

## Architecture at a glance

```
MCP client (Claude, Cursor, custom agent)
        │  stdio / streamable HTTP
        ▼
┌───────────────────────────────┐
│  knowledge-base-mcp (Go)      │
│  ├─ tools: kb_search, kb_get… │
│  ├─ vault: frontmatter, links │
│  ├─ index: full-text (Bleve)  │
│  └─ git: commit / pull / push │
└───────────────┬───────────────┘
                ▼
   local clone of your vault  ⇄  Git remote
```

## Configuration (planned)

| Env var           | Purpose                                              |
|-------------------|------------------------------------------------------|
| `KB_VAULT_PATH`   | Local Git clone of the vault (required)              |
| `KB_GIT_REMOTE`   | Remote URL used to bootstrap‑clone if path is empty  |
| `KB_HTTP_LISTEN`  | Enable HTTP transport, e.g. `127.0.0.1:8765`         |
| `KB_HTTP_TOKEN`   | Bearer token required by the HTTP transport          |
| `KB_INDEX_DIR`    | Where the search index is stored                     |
| `KB_READ_ONLY`    | Disable all write tools                              |

See [`config.example.yaml`](config.example.yaml) for the full set. Real configuration files and tokens are git‑ignored and must never be committed.

## Development

Requirements: Go 1.26+, `git`, and the [OpenSpec](https://github.com/Fission-AI/OpenSpec) CLI for spec‑driven changes.

```bash
openspec list            # active change proposals
openspec validate        # check specs
go build ./...
go test ./...
```

Workflow: every feature starts as an OpenSpec change (`openspec/changes/<name>/`) with a proposal, delta specs, design and tasks; once implemented it is archived into `openspec/specs/`.

## Privacy

This project is public. It must never contain personal notes, real vault paths, repository names of private vaults, tokens or keys. Tests use a synthetic fixture vault under `testdata/`.

## License

[MIT](LICENSE)
