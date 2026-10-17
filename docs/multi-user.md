# Multi-user mode

By default the server serves one vault to whoever holds its token or password. Multi-user mode turns the HTTP transport into a small shared service:

- **accounts** that sign in with a one-time code sent by email, no passwords;
- **several repositories**, each a Git-backed folder of notes with its own clone, index and sync;
- **access rules** per folder: none, read or write, inherited downwards;
- an **admin panel** at `/admin` to manage all three.

It is switched on by one setting, `KB_ADMIN_EMAIL`, together with `KB_HTTP_LISTEN`. Without it nothing changes. The stdio transport always serves one local vault, with no accounts.

| | Classic mode | Multi-user mode |
|---|---|---|
| Who may connect | anyone with the token or the sign-in password | accounts created by the administrator |
| Sign-in | password | 6-digit code sent by email |
| Content | one vault | any number of repositories |
| Paths | `Projects/plan.md` | `<alias>/Projects/plan.md` |
| Who sees what | everything | only folders granted to the account |

## What you need

1. **A mail provider key.** Sign-in codes are delivered through [Resend](https://resend.com). Create an API key with *Sending access* only, restricted to your sending domain: such a key can send mail and nothing else.
2. **A verified sending domain.** Add a domain (a subdomain such as `mail.example.com` is a good choice) in the provider's dashboard and create the DNS records it shows. Without a verified domain the provider's test sender delivers only to the address that owns the provider account, so nobody but you could sign in.
3. **A secret key** that encrypts repository credentials at rest: `openssl rand -base64 32`. Keep it with your other deployment secrets. If it is lost, the credentials have to be entered again; nothing else is encrypted with it.
4. **The administrator's email address.**

## Configuration

Add to the environment of the server (for Docker Compose, to `.env`, which is git-ignored):

```dotenv
KB_HTTP_LISTEN=0.0.0.0:8765
KB_PUBLIC_URL=https://kb.example.com
KB_ADMIN_EMAIL=admin@example.com
KB_RESEND_API_KEY=re_your_sending_only_key
KB_MAIL_FROM="KB <login@mail.example.com>"
KB_SECRET_KEY=<output of: openssl rand -base64 32>
KB_VAULT_ALIAS=main
```

The server refuses to start, naming the setting, if one of the required values is missing or malformed. Two of them, `KB_RESEND_API_KEY` and `KB_SECRET_KEY`, are read from the environment only and are never written to a file or a log by the server. The full reference is in [configuration.md](configuration.md#multi-user-mode).

`KB_HTTP_TOKEN` becomes optional. If you keep it, it is a **recovery token**: it works as a bearer token of the administrator account and as a way into the admin panel when mail cannot be delivered. `KB_OAUTH_PASSWORD` is ignored.

## First run

1. Open `https://kb.example.com/admin`, enter the administrator address and the code from the email. (If mail does not arrive yet, open *Mail is not arriving* and use the recovery token.)
2. **Repositories.** If the server already had a vault (`KB_VAULT_PATH` / `KB_GIT_REMOTE`), it is listed under the alias from `KB_VAULT_ALIAS` and you already hold a write rule on it. To add another one, give it an alias, the remote URL and a credential:
   - an `https://` URL with an **access token** (on GitHub: a fine-grained token limited to that repository with *Contents: read and write*), or
   - an SSH URL (`git@github.com:owner/repo.git`) with an **SSH key**: let the server generate one, register the public key it shows as a deploy key with write access, then press *The key is registered, connect*; or paste a private key of your own (without a passphrase).
3. **Grant yourself access** if you want to read the new repository through your assistant: *Access rules*, pick the folder (the repository root for everything), press `read` or `write` in your own row. A repository you add is visible to nobody, you included, until a rule says so.
4. **Accounts.** Add the address of each person. They can sign in at once and see nothing yet.
5. **Access.** Open a repository's access rules, walk to the folder you want to share and press a level in the person's row. It applies to their next request.
6. **Clients** connect as before to `https://kb.example.com/mcp` with sign-in required; the sign-in page asks for the email address and then the code. See [clients.md](clients.md).

## Sign-in

- A code has 6 digits, is valid for 10 minutes, works once and only in the browser window that asked for it. Five wrong attempts invalidate it; asking for a new code invalidates the previous one.
- The email contains the code and its lifetime, and no link.
- Accounts are created by the administrator only. The sign-in page answers in exactly the same way for an address with an account, a disabled account and no account at all, and never shows whether the email could be delivered. Delivery problems are in the server log.
- Codes are limited to one per address per minute and five per address per hour, twenty per client network address per hour, and one hundred per hour in total.
- Codes exist in memory only; tokens are stored as hashes. Neither appears in logs.
- Every token belongs to the account that signed in. Disabling or deleting an account, or ending its sessions in the panel, stops its tokens at once.

## Repositories

- **Alias.** 1 to 32 lowercase letters, digits and hyphens, starting with a letter or digit. It is the first part of every path and cannot be changed later.
- **Invisible by default.** Adding a repository creates no rules.
- **Isolation.** A failure, an unreachable remote or an unresolved merge in one repository does not block the others.
- **Status** in the panel:

  | Status | Meaning |
  |---|---|
  | `key_pending` | a generated SSH key waits to be registered; nothing is contacted yet |
  | `cloning` | the first clone is running |
  | `ok` / `offline` / `conflict` | serving; in sync, remote unreachable, or a merge needs a decision |
  | `error` | the clone or the connection failed; the reason is shown |
  | `credential_invalid` | the stored credential cannot be decrypted with the current secret key; enter it again |

- **Credentials** are stored encrypted with `KB_SECRET_KEY` in the state file. They cannot be read back: the panel shows the last four characters of a token, or the public key and fingerprint of an SSH key. A decrypted SSH key exists only as a private temporary file outside the data volume while the server runs.
- **SSH host keys.** The key of a host is recorded the first time the server connects to it. If the host later presents another key, the server refuses to talk to it and the repository shows `error`. To trust only keys you provide, put `known_hosts` lines in `KB_SSH_KNOWN_HOSTS`.
- **Removing** a repository deletes the local clone, the index and every rule for it. The remote is never touched. Local commits are pushed first; if they cannot be, the panel asks for an explicit confirmation.
- **The first repository** (the vault from the classic settings) keeps using the Git credentials of the environment (`KB_GIT_TOKEN` or the mounted SSH key); nothing is copied into the state file.

The state file (`state.json`), the repositories added in the panel (`repos/`), the pinned host keys and the link-signing key live in `KB_DATA_DIR`, which defaults to the index directory. Back up the state file together with `KB_SECRET_KEY`.

## One tree

Every path starts with the repository alias: `notes/Projects/plan.md` is `Projects/plan.md` in the repository `notes`.

- `kb_list` with an empty folder lists the repositories available to the caller.
- Search tools (`kb_search`, `kb_grep`, `kb_query`, `kb_context`, `kb_quick_open`, `kb_tags`) cover all of them; a folder filter narrows them to one repository or a folder in it. Ranking across repositories is approximate, because each has its own index; inside one repository it is exact.
- History tools (`kb_log`, `kb_diff`, `kb_ls_tree`) work inside one repository, named by their folder or path. With several repositories and no folder they fail with `invalid_argument` and list the aliases.
- A write changes one repository. Moving between repositories is refused; links between notes resolve inside their own repository.
- `kb_sync_status`, `kb_sync_now`, `kb_conflicts` and `kb_reindex` take an optional `repository` and report per repository. `kb_info` lists the repositories the caller can read, with no filesystem paths.

## Access rules

A rule gives one account a level on one folder of one repository:

| Level | The account can |
|---|---|
| `none` | do nothing: the folder does not exist for it |
| `read` | list, read, search and see the history |
| `write` | also create, change, move, delete, upload and restore |

A rule applies to its folder and everything below, until a rule on a deeper folder says otherwise: **the deepest rule wins**. Without any rule the level is `none`.

| Rules of the account | Path | Result |
|---|---|---|
| `write` on `notes/Projects`, `none` on `notes/Projects/Secret` | `notes/Projects/a.md` | write |
| same | `notes/Projects/Secret/b.md` | not found |
| `read` on `notes/Shared/Team` only | `notes/Shared/Team/x.md` | read |
| same | `notes/Shared/y.md` | not found |
| `read` on `notes/Work` | `notes/Workshop/a.md` | not found (rules match whole folder names) |
| none at all | anything | not found, and the root listing is empty |

What an account may not read **does not exist** for it. A hidden note and a missing note answer identically (`not_found`), in every tool, resource and file route. A write to a folder the account may only read answers `forbidden`.

- **Listings** show what the account can read plus the folders that lead there. With only `read` on `notes/Shared/Team`, the root shows `notes`, `notes` shows only `Shared`, and `notes/Shared` shows only `Team`.
- **Search, tags and counts** consider readable notes only, so a tag used only in hidden notes is not listed and totals do not hint at hidden matches.
- **Links.** A link to a hidden note is reported as unresolved; backlinks from hidden notes are omitted.
- **History.** A commit is listed only if it touches something readable, with the readable paths only. If it also touched hidden paths its message is withheld and the entry is marked `partial`, because commit messages often name files. The history of a note that was moved out of a hidden folder stops at the move.
- **Signed links** (`kb_file_link`, `kb_upload_link`) are bound to the account that created them and are checked again when used: remove the access and the link stops working.
- **Merges.** A conflict report shows the readable paths and counts the others. Resolving a merge needs `write` on every conflicting path; otherwise it answers `forbidden` and an account with wider access has to do it.
- **The administrator has no access to content of its own.** It manages accounts, repositories and rules, and it sees folder names (never file names or content) in the panel. To read notes it grants itself a rule like anyone else, and that grant is logged. This is a safe default and an audit point, not a barrier: the administrator can always grant itself access.
- **Audit.** Every rule change is logged with the administrator who made it. Commits made through the server carry a `KB-User: <address>` trailer; it is pushed with the commit, so it is visible to everyone who can read that repository on the Git host.
- Rules are attached to folder paths. If a folder is renamed or deleted outside the server, its rules grant nothing and the panel marks them as orphaned so you can clear them.

## Moving an existing server to multi-user mode

1. Update the server and start it once without new settings: nothing changes.
2. Set the values from [Configuration](#configuration) and restart.
3. The vault appears under `main/` (or your `KB_VAULT_ALIAS`), visible to the administrator only. **Paths change**: `Projects/plan.md` becomes `main/Projects/plan.md`.
4. Every connected app has to sign in again, now with the email code. Apps stay registered; tokens issued before the switch are refused.

To go back, remove `KB_ADMIN_EMAIL` and restart. The server serves the original vault with the original paths, and the earlier sign-ins work again. Repositories added in the panel stay on disk under `KB_DATA_DIR/repos`, unused.

## Limits

- Accounts and several repositories exist on the HTTP transport only.
- One administrator, set in the configuration. Repositories are added by the administrator only.
- Rules are per folder, not per file.
- An alias cannot be renamed; remove the repository and add it again under the new alias.
- SSH keys protected by a passphrase are not supported.
- There are no personal API tokens: clients sign in through OAuth. The only static token is the administrator's recovery token.
