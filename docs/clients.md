# Connecting clients

The server speaks MCP over stdio (each client launches its own instance) or over streamable HTTP with a bearer token (one long-running instance shared by several clients). Instances share state exclusively through the Git remote.

## Binary

```bash
go install github.com/vaxann/knowledge-base-mcp/cmd/knowledge-base-mcp@latest
```

Claude Desktop / Claude Code / Cursor (JSON config):

```json
{
  "mcpServers": {
    "kb": {
      "command": "knowledge-base-mcp",
      "env": {
        "KB_VAULT_PATH": "/home/me/.local/share/kb/vault",
        "KB_GIT_REMOTE": "git@github.com:me/my-notes.git"
      }
    }
  }
}
```

Claude Code CLI:

```bash
claude mcp add kb -e KB_VAULT_PATH=/home/me/.local/share/kb/vault -e KB_GIT_REMOTE=git@github.com:me/my-notes.git -- knowledge-base-mcp
```

The first start clones the remote using your normal Git credentials (SSH agent, credential helper).

## Container

The image ships `git` and `openssh-client` and exposes no ports. Two volumes: the clone (`/data/vault`) and the index (`/data/index`). The entrypoint starts as root only to read mounted secrets and fix volume ownership, then drops to the unprivileged `kb` user (uid 10001) before starting the server. Pass `--user $(id -u)` instead if you prefer the container to run as your own user; then mounted volumes must be writable by that uid and an SSH key must be readable by it.

### With an SSH deploy key

```json
{
  "mcpServers": {
    "kb": {
      "command": "docker",
      "args": [
        "run", "-i", "--rm",
        "-v", "kb-vault:/data/vault",
        "-v", "kb-index:/data/index",
        "-v", "/home/me/.ssh/kb_deploy_key:/run/secrets/ssh_key:ro",
        "-e", "KB_GIT_REMOTE=git@github.com:me/my-notes.git",
        "ghcr.io/vaxann/knowledge-base-mcp:latest"
      ]
    }
  }
}
```

The entrypoint copies the key to a private location, pins GitHub's host keys (override with `/run/secrets/known_hosts` or `KB_SSH_KNOWN_HOSTS` for other hosts) and sets `GIT_SSH_COMMAND`. The deploy key needs write access.

### With an HTTPS token

```json
{
  "mcpServers": {
    "kb": {
      "command": "docker",
      "args": [
        "run", "-i", "--rm",
        "-v", "kb-vault:/data/vault",
        "-v", "kb-index:/data/index",
        "-e", "KB_GIT_REMOTE=https://github.com/me/my-notes.git",
        "-e", "KB_GIT_TOKEN",
        "ghcr.io/vaxann/knowledge-base-mcp:latest"
      ]
    }
  }
}
```

`-e KB_GIT_TOKEN` (no value) forwards the variable from the launching environment, so the token never appears in the config file. The server passes it to Git through an in-memory credential helper; it is not written to the volume or logs. Use a fine-grained token limited to the vault repository with *Contents: read and write*.

Images are published for `linux/amd64` and `linux/arm64`.

## Long-running instance with Docker Compose (HTTP + token)

```bash
git clone https://github.com/vaxann/knowledge-base-mcp && cd knowledge-base-mcp
cp .env.example .env            # set KB_GIT_REMOTE, KB_HTTP_TOKEN (openssl rand -hex 32), KB_SSH_KEY_FILE or KB_GIT_TOKEN
docker compose up -d --build    # first start clones the vault and builds the index
docker compose logs -f kb
curl -fsS http://127.0.0.1:8765/healthz
```

Rebuild after pulling a new version: `docker compose build --pull && docker compose up -d`. Rotate the token by changing `.env` and `docker compose up -d`.

Clients connect to `http://<host>:8765/mcp` with `Authorization: Bearer <token>`. Without the header the server answers `401` and runs nothing.

Claude Code:

```bash
claude mcp add --transport http kb http://127.0.0.1:8765/mcp --header "Authorization: Bearer $KB_HTTP_TOKEN"
```

Claude Desktop / Cursor (JSON):

```json
{
  "mcpServers": {
    "kb": {
      "type": "http",
      "url": "http://127.0.0.1:8765/mcp",
      "headers": { "Authorization": "Bearer <token>" }
    }
  }
}
```

**Exposing it beyond localhost.** The Compose file publishes the port on `127.0.0.1` only. To reach it from other machines, either put a TLS reverse proxy (Caddy, nginx, Traefik) in front and keep the bind on loopback, join the host to a private network (Tailscale, WireGuard) and set `KB_BIND` to that interface, or set `KB_HTTP_TLS_CERT`/`KB_HTTP_TLS_KEY` and bind `0.0.0.0`. Never publish the plain-HTTP port on a public interface: the token would travel in clear text.

## Several instances at once

Two agents on two machines each run their own instance with their own clone. Every write pulls first and pushes right after, so normal operation is conflict-free. When two instances change the same lines within the same few seconds, the second one to push gets a frozen merge (see [conflicts.md](conflicts.md)).
