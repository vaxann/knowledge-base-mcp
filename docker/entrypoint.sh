#!/bin/sh
# Prepares credentials and volume ownership, then runs the server as the
# unprivileged 'kb' user. Everything here goes to stderr: stdout is the MCP
# protocol stream.
set -eu

KB_USER=kb
KB_HOME=/home/kb

if [ "$(id -u)" = "0" ]; then
  # Bind-mounted volumes may belong to another uid; make them writable for kb.
  for d in "${KB_VAULT_PATH:-/data/vault}" "${KB_INDEX_DIR:-/data/index}"; do
    mkdir -p "$d"
    if [ "$(stat -c %u "$d")" != "$(id -u $KB_USER)" ]; then
      chown -R $KB_USER:$KB_USER "$d"
    fi
  done
fi

# SSH key: mount at /run/secrets/ssh_key (any owner/permissions); it is copied
# to kb's private ~/.ssh so the mounted file's mode does not matter.
key="${KB_SSH_KEY_FILE:-/run/secrets/ssh_key}"
if [ -f "$key" ]; then
  mkdir -p "$KB_HOME/.ssh"
  priv="$KB_HOME/.ssh/id_kb"
  cp "$key" "$priv"
  hosts="$KB_HOME/.ssh/known_hosts"
  if [ -f /run/secrets/known_hosts ]; then
    cp /run/secrets/known_hosts "$hosts"
  elif [ -n "${KB_SSH_KNOWN_HOSTS:-}" ]; then
    printf '%s\n' "$KB_SSH_KNOWN_HOSTS" > "$hosts"
  else
    # Default to GitHub's host keys; set KB_SSH_KNOWN_HOSTS for other hosts.
    ssh-keyscan -t ed25519,rsa github.com > "$hosts" 2>/dev/null || true
  fi
  chown -R $KB_USER:$KB_USER "$KB_HOME/.ssh" 2>/dev/null || true
  chmod 700 "$KB_HOME/.ssh"
  chmod 600 "$priv" "$hosts"
  export GIT_SSH_COMMAND="ssh -i $priv -o IdentitiesOnly=yes -o UserKnownHostsFile=$hosts -o StrictHostKeyChecking=yes"
  echo "entrypoint: using SSH key from $key" >&2
fi

# HTTPS token: KB_GIT_TOKEN is consumed by the server's in-memory credential
# helper; it is never written to disk.
if [ -n "${KB_GIT_TOKEN:-}" ]; then
  echo "entrypoint: using HTTPS token from KB_GIT_TOKEN" >&2
fi

if [ "$(id -u)" = "0" ]; then
  exec su-exec $KB_USER:$KB_USER "$@"
fi
exec "$@"
