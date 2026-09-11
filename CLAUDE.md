# Knowledge Base MCP — guidance for AI coding agents

## Project
Go MCP server exposing a Git-backed, Obsidian-compatible Markdown vault: search, read, write, versioning.
Module: `github.com/vaxann/knowledge-base-mcp`. Go 1.26+.

## Workflow
- Spec-driven with OpenSpec: `/opsx:propose`, `/opsx:apply`, `/opsx:archive`. Read `openspec/` before changing behavior.
- All repository content (specs, code, comments, docs, commits) is in **English**. Discussion with the maintainer may happen in other languages; that never leaks into the repo.

## Privacy and security (this repo is public)
- Never reference the maintainer's private vault: no repository name, no folder names, no note titles, no personal data.
- Never commit tokens, keys, real paths, or real configuration. `config.yaml`, `.env` are git-ignored; only `config.example.yaml` with placeholders is tracked.
- Tests use the synthetic fixture vault under `testdata/`. Do not copy real notes into fixtures.
- Logs must not contain note bodies or secrets.

## Code conventions
- Standard library first; keep dependencies few and pinned.
- `gofmt`, `go vet`, `golangci-lint` clean. Table-driven tests.
- Tool names are prefixed `kb_`; errors return a stable `code` field.
- Never run destructive git operations (force push, reset --hard, amend) in the vault or in this repo.
