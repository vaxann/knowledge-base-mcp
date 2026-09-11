# Connecting clients

The server speaks MCP over stdio only. Every client launches its own instance; instances share state exclusively through the Git remote.

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

## Several instances at once

Two agents on two machines each run their own instance with their own clone. Every write pulls first and pushes right after, so normal operation is conflict-free. When two instances change the same lines within the same few seconds, the second one to push gets a frozen merge (see [conflicts.md](conflicts.md)).
