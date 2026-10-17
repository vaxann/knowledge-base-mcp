# Configuration

Settings come from three layers, each overriding the previous one:

1. built-in defaults,
2. an optional YAML file (`-config path` flag or `KB_CONFIG` env var; see [`config.example.yaml`](../config.example.yaml)),
3. `KB_*` environment variables.

| Env var | YAML key | Default | Meaning |
|---|---|---|---|
| `KB_VAULT_PATH` | `vault.path` | — (required) | Absolute path of the server's private Git clone. Must be the top level of a work tree on the configured branch. |
| `KB_GIT_REMOTE` | `vault.remote` | — | Remote to clone when the path is missing or empty. |
| `KB_EXCLUDE` | `vault.exclude` | `[]` | Extra glob patterns to hide (comma-separated in env). Dot-files and dot-folders are always hidden. |
| `KB_GIT_BRANCH` | `git.branch` | `main` | The only branch the server touches. |
| `KB_GIT_AUTHOR_NAME` / `KB_GIT_AUTHOR_EMAIL` | `git.author_name` / `git.author_email` | `Knowledge Base MCP` / `kb-mcp@localhost` | Commit identity, so server edits are distinguishable in history. |
| `KB_INSTANCE_ID` | `git.instance_id` | `<hostname>-<pid>` | Written as the `KB-Instance:` commit trailer. |
| `KB_GIT_AUTO_PUSH` | `git.auto_push` | `true` | Push after commits. |
| `KB_PUSH_DEBOUNCE` | `git.push_debounce` | `5s` | Coalesce pushes of commits made within this window. |
| `KB_PULL_INTERVAL` | `git.pull_interval` | `60s` | Periodic pull; `0` disables it (writes still pull first). |
| `KB_PULL_FRESHNESS` | `git.pull_freshness` | `5s` | Skip the pull before a write if one succeeded this recently. |
| `KB_GIT_TOKEN` | — | — | HTTPS token handed to Git through an in-memory credential helper. Never stored on disk. |
| `KB_GIT_USERNAME` | — | `x-access-token` | Username paired with the token (GitHub accepts `x-access-token`). |
| `KB_INDEX_DIR` | `search.index_dir` | `$XDG_CACHE_HOME/knowledge-base-mcp/index` | Full-text index location. Must be outside the vault. |
| `KB_SEARCH_LANGUAGES` | `search.languages` | `ru,en` | Stemmers applied to titles and bodies. |
| `KB_GREP_MAX_FILE_SIZE` | `search.grep_max_file_size` | `2MB` | Files larger than this are skipped by `kb_grep`. |
| `KB_HTTP_LISTEN` | `server.http.listen` | — (stdio) | `host:port` to serve streamable HTTP at `/mcp`. A token is mandatory unless the host is loopback. |
| `KB_HTTP_TOKEN` | `server.http.token` | — | Bearer token every `/mcp` request must send. Never logged. |
| `KB_OAUTH_PASSWORD` | `server.http.oauth_password` | = token | Password for the OAuth sign-in page (Claude apps). Enables the embedded OAuth server together with `KB_HTTP_LISTEN`. |
| `KB_PUBLIC_URL` | `server.http.public_url` | derived | Public base URL used as OAuth issuer, e.g. `https://kb.example.com`. Derived from `X-Forwarded-Proto`/`Host` when empty. |
| `KB_OAUTH_STATE` | `server.http.oauth_state` | `<index_dir>/oauth-state.json` | Registered OAuth clients and hashed tokens; keeps sign-ins across restarts. |
| `KB_HTTP_TLS_CERT` / `KB_HTTP_TLS_KEY` | `server.http.tls_cert` / `tls_key` | — | Serve HTTPS directly instead of relying on a proxy. |
| `KB_MAX_UPLOAD` | `server.max_upload` | `50MB` | Size limit for `kb_upload_file`, `PUT /files/…` and the upload page. |
| `KB_LINK_TTL` | `server.link_ttl` | `15m` | Default lifetime of signed download/upload links. |
| `KB_READ_ONLY` | `server.read_only` | `false` | Hide and refuse all write tools. |
| `KB_LOG_LEVEL` | `server.log_level` | `info` | `debug`, `info`, `warn`, `error`. Logs go to stderr and never contain note content or credentials. |

Run `knowledge-base-mcp -check` to validate the configuration and the clone without serving.

## Multi-user mode

Setting `KB_ADMIN_EMAIL` together with `KB_HTTP_LISTEN` switches the HTTP transport to multi-user mode: accounts that sign in with a code sent by email, several repositories and folder-level access rules. Without it nothing changes, and stdio always serves one local vault. See [multi-user.md](multi-user.md) for the whole picture.

| Env var | YAML key | Default | Meaning |
|---|---|---|---|
| `KB_ADMIN_EMAIL` | `server.accounts.admin_email` | — | Address of the administrator account. Enables multi-user mode. |
| `KB_RESEND_API_KEY` | — | — (required) | API key of the mail provider ([Resend](https://resend.com)) used to deliver sign-in codes. Use a key with sending access only. Environment only; never logged. |
| `KB_MAIL_FROM` | `server.accounts.mail_from` | — (required) | Sender of sign-in emails, e.g. `KB <login@mail.example.com>`. The domain must be verified with the mail provider. |
| `KB_SECRET_KEY` | — | — (required) | Base64 of 32 random bytes (`openssl rand -base64 32`) used to encrypt repository tokens and SSH keys at rest. Environment only; never logged or stored. |
| `KB_DATA_DIR` | `server.data_dir` | = index dir | Where the state file and the repositories added in the admin panel live. |
| `KB_VAULT_ALIAS` | `vault.alias` | `main` | Alias under which the vault from `KB_VAULT_PATH` / `KB_GIT_REMOTE` is imported as the first repository. |
| `KB_SSH_KNOWN_HOSTS` | `git.ssh_known_hosts` | — | `known_hosts` lines. When set, SSH repositories trust only these hosts; otherwise host keys are pinned on first contact. |

In multi-user mode `KB_VAULT_PATH` becomes optional (the server may start with no repository), `KB_HTTP_TOKEN` becomes optional and acts as a recovery token of the administrator account, and `KB_OAUTH_PASSWORD` is ignored. The server refuses to start, naming the setting, when one of the required values is missing or malformed.

## Error codes

Every tool failure is a tool error whose JSON payload has a stable `code` and a `message`:

| Code | Meaning |
|---|---|
| `invalid_path` | Absolute path, `..` segment, symlink escape, or a note path without `.md`. In multi-user mode also a file path without a repository alias in front. |
| `not_found` | Missing or excluded path, unknown revision, missing heading. In multi-user mode also every path the account may not read, and an unknown repository alias: hidden and missing are reported alike. |
| `already_exists` | Create/move target exists. |
| `unsupported_file` | Write attempted on a non-Markdown file. |
| `conflict` | The `etag` is stale; the payload carries the current `content` and `etag`. |
| `merge_conflict` | A merge with the remote is frozen; payload carries `conflicts` (base/ours/theirs) and `remote_commits`. |
| `conflict_markers_present` | A resolution still contains `<<<<<<<` markers. |
| `patch_failed` | A patch operation could not be applied; nothing was written. |
| `read_only` | The server runs in read-only mode. |
| `forbidden` | Multi-user mode: a write to a folder the account may only read, a merge resolution without write access to every conflicting path, or `kb_reindex` by an account other than the administrator. |
| `invalid_argument` | Bad glob, regex, date, query or operation; signed links requested without a public URL. In multi-user mode also a move between repositories, and a history tool called without a folder when several repositories are available. |
| `too_large` | File above `max_bytes` (`kb_get_file`) or above `KB_MAX_UPLOAD`. |
| `no_remote` / `not_in_conflict` | `kb_sync_now` without a remote; `kb_resolve_conflict` without a frozen merge. |
