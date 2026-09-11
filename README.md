# Knowledge Base MCP

An [MCP](https://modelcontextprotocol.io) server, written in Go, that gives AI assistants fast, versioned access to a personal knowledge base kept as an Obsidian‑compatible Markdown vault inside a Git repository.

> **Status:** design phase. The requirements live in [`openspec/`](openspec/) and are being refined before implementation starts. Nothing is runnable yet.

## What it will do

- **Fast search, three ways** — ranked full-text search with Cyrillic and Latin stemming and frontmatter filters; grep-style exact or regex matching across all notes; Dataview-style metadata queries. A "context bundle" tool returns the most relevant sections within a size budget, so any LLM can answer questions about the vault with a single call.
- **Read notes** — get a note with parsed YAML frontmatter, headings, tags, outgoing links and backlinks; list folders; query notes by frontmatter fields (Dataview‑style).
- **Edit notes** — create, replace or patch content (append, replace a section, set frontmatter keys), rename and delete, with optimistic concurrency. The server is a data-access layer: no templates, no magic link rewriting, the calling model stays in control.
- **Versioning for free** — every change is an atomic Git commit under a dedicated author on `main`; the server pulls and pushes automatically and lets clients walk the log, list the tree at any revision, read and diff old versions (deleted notes included) and restore them. History is never rewritten.
- **Runs anywhere your agent runs** — stdio only, as a binary or a container. Every client gets its own instance with a private clone; the Git remote is the only shared state, and conflicts are resolved automatically without losing either side.
- **Consistent cards** — frontmatter must be valid YAML on every write, and folders can carry a `_schema.yaml` (required fields, enums, file-name pattern) that the server enforces or warns about.
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
│  ├─ schema: YAML + _schema    │
│  ├─ index: full-text (Bleve)  │
│  └─ git: commit / pull / push │
└───────────────┬───────────────┘
                ▼
   private clone of your vault  ⇄  Git remote  ⇄  your editor's clone
```

## Configuration (planned)

| Env var           | Purpose                                              |
|-------------------|------------------------------------------------------|
| `KB_VAULT_PATH`   | Local Git clone of the vault (required)              |
| `KB_GIT_REMOTE`   | Remote URL used to bootstrap‑clone if path is empty  |
| `KB_GIT_CONFLICT` | `remote-wins` (default) or `local-wins`              |
| `KB_SCHEMA_MODE`  | `strict` (default) or `warn`                         |
| `KB_INDEX_DIR`    | Where the search index is stored                     |
| `KB_READ_ONLY`    | Disable all write tools                              |

See [`config.example.yaml`](config.example.yaml) for the full set. Real configuration files and credentials are git‑ignored and must never be committed.

## Roadmap

After the first release: text extraction from PDF and Office attachments so search covers documents too, and semantic (embedding-based) search next to full text.

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
