# Design

## Context

See `proposal.md` for motivation. The constraints that shape the approach:

- `kb.Service` owns exactly one vault: one clone, one index, one catalog, one write lock, one sync loop. All 30 tools call it directly from `internal/mcpserver`.
- `internal/oauth` is a self-contained authorization server with one shared password, a JSON state file, and a boolean `Valid(token)`; nothing downstream knows who is calling.
- The MCP SDK already carries an authenticated identity: an HTTP middleware can attach `auth.TokenInfo` (with a `UserID`) to the request, the streamable handler binds the MCP session to that user id, and tool handlers see it in `req.Extra.TokenInfo`.
- Project rules: standard library first, no secrets or note bodies in logs, nothing personal in the public repository, classic single-vault users must not be broken.

## Goals / Non-Goals

**Goals:**
- One enforcement point for access rules that a new tool cannot bypass by accident.
- Classic mode stays byte-for-byte compatible: same paths, same tool results, same state file.
- `kb.Service` stays a single-vault component; multi-repository behavior is composed above it.
- No new module dependencies.

**Non-Goals:**
- Strictly comparable relevance scores across repositories.
- Horizontal scaling or several server processes over one data directory.
- A JavaScript application for the admin panel.
- Hiding the existence of a repository alias from an account that has a rule somewhere inside it.

## Decisions

### 1. Two modes selected by one setting

`KB_ADMIN_EMAIL` on the HTTP transport selects multi-user mode; everything else is classic. `main` builds one of two object graphs behind the same interface consumed by `mcpserver`.

*Alternative:* make every deployment multi-user with an implicit single account. Rejected: it would force alias-prefixed paths and mail configuration on stdio and single-user installs for no benefit.

### 2. A workspace layer above `kb.Service`

New package `internal/workspace` exposes the operations the tools need (the current `Service` method set) and is the only thing `mcpserver` talks to.

- Classic: a flat workspace wrapping one `Service`, no alias, no rules.
- Multi-user: a registry `alias -> *kb.Service`. For each call it resolves the caller, splits the alias off the path, builds a scope for (caller, repository), delegates, and re-prefixes returned paths. Fan-out operations (search, grep, query, quick open, context, tags, status) run per repository concurrently and merge.

Each repository gets its own `Service` built from a derived `config.Config` (vault path, remote, branch, token, index directory overridden; everything else shared), under `<data_dir>/repos/<id>/{vault,index}` where `<id>` is a random identifier independent of the alias. `KB_DATA_DIR` defaults to the index directory, which is already the server-owned volume, so no new volume is needed. The imported first repository records its existing vault and index paths and keeps them.

*Alternative:* teach `Service` about several vaults. Rejected: it would entangle locks, sync loops and conflict state that are naturally per clone.

*Alternative:* one shared search index with a repository field. Rejected: `Service` owns index lifecycle (stamping, reindex, incremental refresh), and removing a repository becomes a directory delete instead of a bulk index deletion. The cost is that merged scores come from separate indexes; ties and near-ties may order differently than a single index would. Accepted.

### 3. Access is enforced inside `Service` through a scope in the context

`kb.Scope` is a small interface: `CanRead(path)`, `CanWrite(path)`, `Traversable(folder)`. The workspace puts one in the context; a missing scope means full access and occurs only in classic mode. In multi-user mode every call carries a scope, the administrator's included, built from that account's rules. Every `Service` method consults it:

- path-addressed reads and writes check it first and return `not_found` or `forbidden`;
- listing filters entries and uses `Traversable` for pass-through folders;
- catalog-based operations (query, tags, backlinks, quick open, link resolution) filter by `CanRead`; links are resolved over the whole repository first and then redacted, so resolution matches what an editor shows;
- full-text search takes the predicate into the hit loop: it pages through index hits until enough readable ones are collected, and totals count readable hits only;
- history filters changed paths, withholds messages of mixed commits, and limits whole-repository diffs to readable changed paths;
- conflict reporting filters paths and counts the rest.

Rule evaluation itself lives in a pure package `internal/acl` (longest-prefix match on path segments, plus "is there a readable rule below this folder" for traversal), with table-driven tests. It has no administrator override: the administrator role decides who may manage the server, not who may read notes.

*Alternative:* enforce only in the workspace wrapper by post-filtering results. Rejected: pagination, totals, tag counts and backlinks cannot be made correct or leak-free from outside.

A leak test matrix backs this up: a fixture repository with a hidden folder containing a unique marker string and unique file names; the test calls every registered tool and resource with a restricted caller and asserts that neither the marker nor any hidden name appears in any result or error. The matrix is driven from the tool registry so a new tool is covered automatically or fails the test.

### 4. Caller identity flows through the SDK

The bearer check becomes a token verifier that returns the account id; the SDK middleware attaches it and the session is pinned to that user. A helper in `mcpserver` turns `req.Extra.TokenInfo` into a principal (id, address, administrator flag), re-read from the store on every call so disabling an account or changing a rule is effective at once. The static token maps to the administrator principal and inherits that account's rules. Classic mode and stdio use a fixed full-access principal.

Administrative operations do not go through scopes. The admin panel reads folder names (never file names or content) straight from the repository to draw the rule tree, and `kb_reindex` for the administrator returns status only, so neither depends on, or widens, the administrator's content access.

### 5. One state store

New package `internal/store`: a versioned JSON document with accounts, rules, repositories and OAuth data, guarded by a mutex, written atomically (temporary file, fsync, rename, mode `0600`) with the previous version kept as a backup file. Data volume is tens of accounts and hundreds of rules; a database is not justified.

`internal/oauth` gets a small storage interface with two implementations: its current file (classic, unchanged format) and the store section (multi-user). Token records gain an account id. On the first multi-user start, registered clients are copied from the classic file and tokens are not; the classic file is left in place so switching back restores classic sessions.

*Alternative:* bbolt (already an indirect dependency). Rejected for now: adds a direct dependency and binary state for no measurable gain at this size.

### 6. Email sign-in

- `internal/oauth` takes an authenticator: password (classic) or email code (multi-user). The authorization, PKCE, token and refresh code is shared.
- Codes: six digits drawn uniformly from `crypto/rand`, kept in memory only as a salted hash, keyed by the sign-in request id together with address, expiry and attempt counter. An unknown or disabled address gets a decoy entry, so the verification path and timing are the same. Failures are delayed like wrong passwords today. A restart drops pending codes; the person requests a new one.
- Mail is sent asynchronously so page timing does not depend on the provider. `internal/mail` defines a `Sender` interface; the Resend implementation is a single `POST https://api.resend.com/emails` with `Authorization: Bearer`, a `User-Agent` (the API rejects requests without one), an `Idempotency-Key`, a text and a minimal HTML body, and a 10 s timeout. Tests use a fake sender and an `httptest` server.
- Limits: per address 1 per minute and 5 per hour; per client address 20 per hour; 100 per hour in total. The client address is the peer address, or the last `X-Forwarded-For` entry when the peer is loopback or private (a reverse proxy or tunnel on the host).

*Alternative:* a magic link. Rejected: links in mail train people to click, mail scanners pre-fetch them, and the OAuth flow starts in an in-app browser that the link would not return to.

### 7. Repository credentials

A repository has one of three credential kinds: `token` (HTTPS), `ssh_key` (SSH), or `host` (only the imported first repository: whatever the process environment already provides, an environment token or a mounted key, left exactly as it is).

Stored secrets use AES-256-GCM from the standard library, key from `KB_SECRET_KEY` (base64 of 32 random bytes), random nonce per value, repository id as associated data so a ciphertext cannot be moved to another entry.

- *Token:* handed to Git through the existing in-memory credential helper; never on disk in clear.
- *SSH key:* Git needs a key file, so the decrypted key is written with mode `0600` into a private directory created with `os.MkdirTemp` outside the data volume, recreated at start and removed on repository removal and shutdown. Each Git command for that repository runs with its own `GIT_SSH_COMMAND` (`-i <file>`, `IdentitiesOnly=yes`, no agent, the server's known-hosts file), overriding any process-wide one.
- *Generation and validation:* `ssh-keygen` creates an ed25519 pair per repository and derives the public key and fingerprint of a supplied key (which also rejects malformed and passphrase-protected keys). A generated key puts the repository in `key_pending`; nothing is attempted until the administrator confirms the public key is registered.
- *Host keys:* one known-hosts file in the data directory. With `KB_SSH_KNOWN_HOSTS` set it is seeded from it and checking is strict; otherwise unknown hosts are accepted on first contact and pinned (`accept-new`). A changed key fails the connection and surfaces as `error`.

URL validation is strict because the value reaches the Git command line: `https://` without user info for tokens; `ssh://` or scp-like `user@host:path` matched against a conservative pattern for keys; everything else, including values starting with `-` and `ext::`/`file://` transports, is rejected.

*Alternative:* write the OpenSSH key format in Go, or add `golang.org/x/crypto/ssh`. Rejected: `ssh` must be present for SSH remotes anyway, so `ssh-keygen` costs nothing and avoids both a dependency and hand-written key serialisation. When the binary is missing, adding an SSH repository fails with a clear message and HTTPS repositories are unaffected.

*Alternative:* keep decrypted keys on the data volume. Rejected: volume backups would then contain clear-text keys, defeating encryption at rest.

### 8. Repository lifecycle

States: `key_pending` (generated SSH key not yet confirmed), `cloning`, then the service's own `ok` / `offline` / `conflict`, plus `error` and `credential_invalid`. A new repository gets no rules; the one-time import of the configured vault adds a single ordinary `write` rule for the administrator on its root so a migrated deployment is not empty for its owner. Adding writes the registry entry first and clones in a goroutine; a failed clone leaves the entry for correction. Removing closes the service, attempts a final push, deletes the directories (for the imported repository, the contents of its mount points), then deletes its rules and entry in one store update. A data-directory lock complements the per-index lock so two processes cannot share a registry.

### 9. Signed links

Signing moves to the workspace with one key. The signed string gains the issuing account id; redemption rebuilds the scope for that account and checks the level again. Classic links keep their current format.

### 10. Admin panel

Package `internal/admin`: server-rendered pages with `html/template` and embedded static files, forms with post-redirect-get, no client framework and no inline scripts. Folder navigation is a breadcrumb plus the list of subfolders, each marked when rules exist at or below it; the chosen folder shows a table of accounts with explicit and effective levels. Sessions are random identifiers held in memory (hash only), delivered in a `__Host-` cookie with `HttpOnly`, `Secure`, `SameSite=Strict`; each session has an anti-forgery token checked together with the `Origin` header. A restart signs the administrator out, which is acceptable.

### 11. Tool surface in multi-user mode

Tool names do not change. `kb_sync_status`, `kb_sync_now`, `kb_conflicts` and `kb_reindex` gain an optional `repository` argument and return per-repository results; `kb_info` returns the repository list. `forbidden` is added to the stable error codes. Write tools stay listed for everyone; an account without write access gets `forbidden` or `not_found` per call. Server instructions sent at `initialize` describe the alias-prefixed tree in multi-user mode.

### 12. Delivery order

Three phases, each leaving a working server:

1. Accounts and email sign-in, plus the rule model with repository-level evaluation only: an account with a readable rule on the repository root sees the repository, everyone else sees nothing. The configured vault is imported with the administrator's rule.
2. Workspace, registry and unified tree. Repositories added in this phase are invisible until a rule is written to the store.
3. Folder-level enforcement in every tool, and the admin panel.

Nothing is released between phases, so the path change in phase 2 reaches multi-user deployments together with the rest.

## Risks / Trade-offs

- [A code path forgets the scope and leaks hidden content] → enforcement lives in `Service`, not in tool handlers; the registry-driven leak matrix fails when a tool is not covered.
- [Commit messages written outside the server mention hidden names] → messages of commits that touch any unreadable path are withheld; a commit confined to readable folders shows its message as written. Accepted and documented.
- [Mail outage or a misconfigured sending domain locks everyone out of new sign-ins] → existing sessions keep working; the static token gives the administrator MCP and panel access; the docs state that the provider's test sender only delivers to the provider account's own address.
- [The secret key is lost or replaced] → affected repositories report `credential_invalid` and behave like an offline repository on their last clone until a credential is re-entered; nothing else is encrypted with it.
- [The administrator can still grant itself access to anything] → "invisible by default" is a safe default and an audit point, not a barrier against the administrator; rule changes are logged with the acting account. Documented as such.
- [A decrypted SSH key exists as a file while the server runs] → private temporary directory outside the data volume, mode `0600`, removed on shutdown; a crash can leave it behind until the next start cleans the directory. A host compromised at runtime can read credentials in either form.
- [First-contact pinning trusts the network once] → documented; deployments that need more set `KB_SSH_KNOWN_HOSTS`.
- [State file corruption] → atomic replace plus a backup of the previous version; the server refuses to start on an unreadable state file rather than starting empty and re-importing.
- [Counting readable totals scans all hits of a query] → bounded by repository size; the unrestricted path skips the predicate entirely.
- [Merged ranking across repositories is approximate] → documented; a folder filter gives exact per-repository ranking.
- [The account address in the `KB-User` trailer is pushed to the remote] → visible to anyone with access to that repository on the Git host; documented in the multi-user guide.
- [Rules refer to folders by path, so renaming a folder outside the server detaches its rules] → detached rules grant nothing (deny by default) and are shown as orphaned in the panel.

## Migration Plan

1. Archive `bootstrap-knowledge-base-mcp` so the three modified capabilities exist as main specs.
2. Deploy the new version without new settings: classic mode, no change.
3. To switch: create a sending-only mail API key restricted to a verified domain, generate `KB_SECRET_KEY`, set `KB_ADMIN_EMAIL`, `KB_RESEND_API_KEY`, `KB_MAIL_FROM`, restart. The vault appears under `main/`, visible to the administrator only and still using its current Git credentials; clients sign in again by email; update any saved paths.
4. Rollback: remove `KB_ADMIN_EMAIL` and restart. Classic mode resumes with the original vault, the classic OAuth file and unprefixed paths. Repositories added in the panel stay on disk, unused.

## Open Questions

- Language of the sign-in email and pages: English only at first, or selectable per deployment.
- Exact numeric rate limits, to be tuned after first real use.
