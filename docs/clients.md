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

## Claude apps (iOS, Android, web) and other OAuth-only clients

The Claude apps add remote MCP servers as *custom connectors* and only support OAuth sign-in, not static tokens. The server therefore embeds a small OAuth 2.1 authorization server (RFC 8414/9728 metadata, dynamic client registration, PKCE, refresh tokens). It is on whenever the HTTP transport runs; the sign-in password is `KB_OAUTH_PASSWORD` (falls back to `KB_HTTP_TOKEN`).

1. Expose the server over HTTPS (see below), e.g. `https://kb.example.com`. Set `KB_PUBLIC_URL` to that URL when the proxy does not pass `X-Forwarded-Proto`/`Host` (Cloudflare Tunnel does).
2. In the Claude app: *Add custom connector*, name `kb`, URL `https://kb.example.com/mcp`, *Requires sign-in* on, Client ID and secret empty (the app registers itself).
3. Claude opens the server's sign-in page; type `KB_OAUTH_PASSWORD`. The app receives an access token (24 h) and a refresh token (90 days, rotated on use).

Tokens and registered clients are stored hashed in `oauth-state.json` on the index volume; to revoke everything, delete that file and restart. Every wrong password costs one second, and all MCP calls made by the app carry the connector's `KB-Client` name in commit trailers.

### Ports and paths

| What | Where |
|---|---|
| Container listens | `0.0.0.0:8765` inside the container (`KB_HTTP_LISTEN`) |
| Published on the host | `127.0.0.1:8765` (`KB_BIND`/`KB_PORT` in `.env`), plain HTTP |
| MCP endpoint | `/mcp` (streamable HTTP; bearer token or OAuth access token) |
| Health check | `/healthz` (no auth) |
| OAuth | `/.well-known/oauth-authorization-server`, `/.well-known/oauth-protected-resource`, `/oauth/register`, `/oauth/authorize`, `/oauth/token` |

Everything the tunnel or proxy needs to forward is the single origin `http://127.0.0.1:8765`; all paths above live on it.

### Cloudflare Tunnel

Point the tunnel's public hostname at the host port; Cloudflare terminates TLS.

```bash
cloudflared tunnel login
cloudflared tunnel create kb
cloudflared tunnel route dns kb kb.example.com
```

`~/.cloudflared/config.yml`:

```yaml
tunnel: kb
credentials-file: /home/me/.cloudflared/<tunnel-id>.json
ingress:
  - hostname: kb.example.com
    service: http://127.0.0.1:8765
  - service: http_status:404
```

```bash
cloudflared tunnel run kb          # or: cloudflared service install
curl -fsS https://kb.example.com/healthz
curl -fsS https://kb.example.com/.well-known/oauth-authorization-server | jq .issuer   # must print the public URL
```

The same works with a dashboard-managed tunnel (Zero Trust → Networks → Tunnels → Public hostname → `HTTP`, `127.0.0.1:8765`). To run `cloudflared` inside the Compose stack instead, set the public hostname's service to `http://kb:8765`, put the tunnel token in `.env` as `CLOUDFLARE_TUNNEL_TOKEN` and start it with `docker compose --profile tunnel up -d`. Cloudflare forwards `X-Forwarded-Proto: https` and the public `Host`, so the OAuth metadata advertises `https://kb.example.com` without further configuration; set `KB_PUBLIC_URL` only if the issuer printed above is wrong. Consider a Cloudflare Access policy or WAF rules on top; the server's own token and OAuth still gate every call.

**Exposing it beyond localhost.** The Compose file publishes the port on `127.0.0.1` only. To reach it from other machines, either put a TLS reverse proxy (Caddy, nginx, Traefik) in front and keep the bind on loopback, join the host to a private network (Tailscale, WireGuard) and set `KB_BIND` to that interface, or set `KB_HTTP_TLS_CERT`/`KB_HTTP_TLS_KEY` and bind `0.0.0.0`. Never publish the plain-HTTP port on a public interface: the token would travel in clear text.

## Several instances at once

Two agents on two machines each run their own instance with their own clone. Every write pulls first and pushes right after, so normal operation is conflict-free. When two instances change the same lines within the same few seconds, the second one to push gets a frozen merge (see [conflicts.md](conflicts.md)).
