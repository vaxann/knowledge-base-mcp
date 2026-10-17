# Knowledge Base MCP

An [MCP](https://modelcontextprotocol.io) server, written in Go, that gives AI assistants fast, versioned access to a personal knowledge base kept as a folder of Markdown files inside a Git repository. Works with an Obsidian or Logseq vault or any plain Markdown tree; the server understands frontmatter, wikilinks and tags but imposes no structure on your content.

> **Status:** first implementation complete and covered by tests; not yet released. The requirements live in [`openspec/`](openspec/).

## What it does

- **Fast search, three ways** — ranked full-text search with Cyrillic and Latin stemming and frontmatter filters; grep-style exact or regex matching across all notes; Dataview-style metadata queries. A "context bundle" tool returns the most relevant sections within a size budget, so any LLM can answer questions about the vault with a single call.
- **Read notes** — get a note with parsed YAML frontmatter, headings, tags, outgoing links and backlinks; list folders; query notes by frontmatter fields (Dataview‑style).
- **Edit notes** — create, replace or patch content (append, replace a section, set frontmatter keys), rename and delete, with optimistic concurrency. The server is a data-access layer: no templates, no magic link rewriting, the calling model stays in control.
- **Attachments too** — PDFs, photos and documents can be fetched and stored through MCP, and exchanged with a phone or browser through short-lived signed download/upload links, all committed like notes.
- **Versioning for free** — every change is an atomic Git commit under a dedicated author on `main`; the server pulls and pushes automatically and lets clients walk the log, list the tree at any revision, read and diff old versions (deleted notes included) and restore them. History is never rewritten.
- **Runs anywhere your agent runs** — stdio for desktop clients, or one long-running instance over streamable HTTP protected by a bearer token, with built-in OAuth sign-in for the Claude apps' custom connectors (Docker Compose included), as a binary or a multi-arch container (amd64, arm64). Every instance has a private clone; the Git remote is the only shared state. When a merge conflicts, the server does not guess: it freezes the merge locally, refuses writes with the full conflict (base, ours, theirs), and lets the agent, which has the context, submit the resolution. Nothing is pushed until then and nothing is ever lost.
- **Safe by default** — vault‑relative paths only, deny‑listed folders (`.obsidian/`, `.git/`, …), read‑only mode, no note content or secrets in logs.

## Architecture at a glance

```
MCP client (Claude, Cursor, custom agent)
        │  stdio, or HTTP /mcp + bearer token
        ▼
┌───────────────────────────────┐
│  knowledge-base-mcp (Go)      │   (binary or container)
│  ├─ stdio | HTTP + bearer    │
│  ├─ tools: kb_search, kb_get… │
│  ├─ vault: frontmatter, links │
│  ├─ index: full-text (Bleve)  │
│  └─ git: commit / pull / push │
└───────────────┬───────────────┘
                ▼
   private clone of your vault  ⇄  Git remote  ⇄  your editor's clone
```

## Quick start (binary, stdio)

```bash
go install github.com/vaxann/knowledge-base-mcp/cmd/knowledge-base-mcp@latest
export KB_VAULT_PATH=$HOME/.local/share/kb/vault      # private clone, created on first start
export KB_GIT_REMOTE=git@github.com:me/my-notes.git    # your vault repository
knowledge-base-mcp -check                              # validates config, clones, builds the index
```

Then register `knowledge-base-mcp` as an MCP command in your client (Claude Desktop, Claude Code, Cursor). See [docs/clients.md](docs/clients.md).

## Running in Docker (always-on server)

This is the recommended setup for a home server or VPS: one container that keeps a private clone of the vault in sync with GitHub, serves MCP over HTTP with a token, and lets the Claude and ChatGPT apps sign in through OAuth. The port stays on localhost; a tunnel or reverse proxy on the host exposes it over HTTPS.

```
Claude Code / scripts ──── Bearer token ────┐
Claude app / ChatGPT ───── OAuth sign-in ───┤
                                            ▼
   https://kb.example.com  (Cloudflare Tunnel or reverse proxy on the host)
                                            │ http://127.0.0.1:8765
                                    ┌───────┴────────┐
                                    │ knowledge-base │  docker compose service "kb"
                                    │ -mcp container │  volumes: kb-vault, kb-index
                                    └───────┬────────┘
                                            │ SSH deploy key (or HTTPS token)
                                     git@github.com:me/my-notes.git
```

### 1. Prerequisites

- Docker with Compose v2, `git`, `ssh-keygen`, `openssl`.
- A GitHub repository with your Markdown vault (`main` branch).
- A domain in Cloudflare (optional, only for the mobile apps / remote access).

### 2. Create a deploy key with write access

The container needs its own Git identity. A deploy key is limited to one repository and can be revoked without touching your personal keys.

```bash
mkdir -p ~/.config/knowledge-base-mcp && chmod 700 ~/.config/knowledge-base-mcp
ssh-keygen -t ed25519 -N "" -C "knowledge-base-mcp" -f ~/.config/knowledge-base-mcp/deploy_key
gh repo deploy-key add ~/.config/knowledge-base-mcp/deploy_key.pub --repo me/my-notes --allow-write --title "knowledge-base-mcp"
# or paste deploy_key.pub in GitHub → repository → Settings → Deploy keys, tick "Allow write access"
```

Alternative: an HTTPS fine-grained token with *Contents: read and write* in `KB_GIT_TOKEN` and an `https://` remote.

### 3. Configure

```bash
git clone https://github.com/vaxann/knowledge-base-mcp.git && cd knowledge-base-mcp
cp .env.example .env
openssl rand -hex 32     > ~/.config/knowledge-base-mcp/http_token       # for Claude Code / scripts
openssl rand -base64 18  > ~/.config/knowledge-base-mcp/oauth_password   # typed on the sign-in page
```

Edit `.env` (git-ignored, never commit it):

```dotenv
KB_GIT_REMOTE=git@github.com:me/my-notes.git
KB_GIT_BRANCH=main
KB_SSH_KEY_FILE=/home/me/.config/knowledge-base-mcp/deploy_key
KB_HTTP_TOKEN=<contents of http_token>
KB_OAUTH_PASSWORD=<contents of oauth_password>
KB_PUBLIC_URL=https://kb.example.com     # set once the tunnel/proxy is up; optional
KB_BIND=127.0.0.1
KB_PORT=8765
KB_INSTANCE_ID=home-server
```

### 4. Start

```bash
docker compose up -d --build     # first start clones the vault and builds the index (a minute for ~1k notes)
docker compose logs -f kb        # wait for "serving MCP over HTTP"
curl -fsS http://127.0.0.1:8765/healthz
```

What you get on `127.0.0.1:8765`:

| Path | Purpose |
|---|---|
| `/mcp` | MCP endpoint (streamable HTTP); needs `Authorization: Bearer <KB_HTTP_TOKEN>` or an OAuth access token |
| `/healthz` | liveness, no auth |
| `/.well-known/oauth-*`, `/oauth/*` | OAuth metadata, client registration, sign-in page, token endpoint |
| `/files/<path>`, `/upload/<path>` | attachment download / upload (token or signed link) |
| `/admin` | admin panel, in [multi-user mode](docs/multi-user.md) only |

The container runs as an unprivileged user, holds the clone and the index in the named volumes `kb-vault` and `kb-index`, pulls from GitHub every 60 s and before every write, and pushes each commit.

### 5. Connect clients

Claude Code on the same host:

```bash
claude mcp add --scope user --transport http kb http://127.0.0.1:8765/mcp \
  --header "Authorization: Bearer $(cat ~/.config/knowledge-base-mcp/http_token)"
```

Claude app (iOS/Android/web) and ChatGPT need the server on HTTPS. With Cloudflare Tunnel running on the host, add a public hostname `kb.example.com` → `HTTP` `127.0.0.1:8765`, then check:

```bash
curl -fsS https://kb.example.com/healthz
curl -fsS https://kb.example.com/.well-known/oauth-authorization-server | jq .issuer   # https://kb.example.com
```

- Claude app: *Add custom connector* → URL `https://kb.example.com/mcp`, *Requires sign-in* on, Client ID/secret empty → enter `KB_OAUTH_PASSWORD` on the sign-in page.
- ChatGPT (web, developer mode): Apps → create → URL `https://kb.example.com/mcp`, authentication OAuth, no client credentials.

Details, alternatives (reverse proxy, direct TLS) and the file tools are in [docs/clients.md](docs/clients.md).

### 6. Day-to-day operations

```bash
docker compose logs -f kb                              # logs (paths and tool names only, never note content)
docker compose build --pull && docker compose up -d    # update after git pull
docker compose down                                    # stop; volumes and tokens survive
docker compose down -v                                 # wipe clone, index, OAuth state (re-clones on next start)
```

- Rotate the token or password: edit `.env`, `docker compose up -d`, reconnect clients.
- Revoke all app sign-ins: `docker compose exec kb rm /data/index/oauth-state.json && docker compose restart kb`.
- Merge conflicts are never resolved by the server: `kb_sync_status` shows `conflict`, and the next client to write is asked to resolve it (see [docs/conflicts.md](docs/conflicts.md)).
- Uploads are limited to `KB_MAX_UPLOAD` (50 MB); Cloudflare's free plan caps request bodies at 100 MB.

## Sharing with other people: multi-user mode

One server can serve several people and several repositories, with access granted per folder:

- accounts sign in with a one-time code sent by email (no passwords);
- each repository is a Git-backed folder of notes with its own clone, index and sync;
- an account sees only the folders it was granted, read-only or read-write, and everything else does not exist for it;
- an admin panel at `/admin` manages accounts, repositories and access rules.

It is off by default and switched on by `KB_ADMIN_EMAIL`. You need a [Resend](https://resend.com) API key with sending access, a verified sending domain and a secret key:

```dotenv
KB_ADMIN_EMAIL=admin@example.com
KB_RESEND_API_KEY=re_your_sending_only_key
KB_MAIL_FROM="KB <login@mail.example.com>"
KB_SECRET_KEY=<output of: openssl rand -base64 32>
```

After `docker compose up -d` open `https://kb.example.com/admin`. The existing vault becomes the first repository under the alias `main`, so paths gain that prefix (`main/Projects/plan.md`), and connected apps sign in again with the email code. Removing `KB_ADMIN_EMAIL` brings the single-vault server back as it was. Everything else is in [docs/multi-user.md](docs/multi-user.md).

## Tools

| Group | Tools |
|---|---|
| Read | `kb_get_note`, `kb_get_section`, `kb_list`, `kb_backlinks`, `kb_tags` |
| Search | `kb_search`, `kb_grep`, `kb_query`, `kb_context`, `kb_quick_open` |
| Write | `kb_create_note`, `kb_replace_note`, `kb_patch_note`, `kb_move_note`, `kb_delete_note`, `kb_restore`, `kb_resolve_conflict` |
| History & sync | `kb_log`, `kb_history`, `kb_ls_tree`, `kb_show_revision`, `kb_diff`, `kb_sync_status`, `kb_sync_now`, `kb_conflicts` |
| Files | `kb_get_file`, `kb_upload_file`, `kb_file_link`, `kb_upload_link` |
| Ops | `kb_info`, `kb_reindex` |

Resources: `kb://note/<path>` (Markdown), `kb://folder/<path>` (JSON listing) and `kb://file/<path>` (binary blob).

## Configuration

| Env var           | Purpose                                              |
|-------------------|------------------------------------------------------|
| `KB_VAULT_PATH`   | Local Git clone of the vault (required)              |
| `KB_GIT_REMOTE`   | Remote URL used to bootstrap‑clone if path is empty  |
| `KB_GIT_TOKEN`    | HTTPS token handed to Git (alternative to an SSH key)|
| `KB_INDEX_DIR`    | Where the search index is stored                     |
| `KB_HTTP_LISTEN`  | Serve HTTP at `/mcp` instead of stdio, e.g. `0.0.0.0:8765` |
| `KB_HTTP_TOKEN`   | Bearer token required on `/mcp` (mandatory off-loopback) |
| `KB_READ_ONLY`    | Disable all write tools                              |
| `KB_ADMIN_EMAIL`  | Administrator address; switches on [multi-user mode](docs/multi-user.md) |

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
