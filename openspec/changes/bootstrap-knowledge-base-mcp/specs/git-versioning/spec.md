## Purpose

Guarantees that every change to the vault is a reviewable Git commit, keeps the local clone synchronised with its remote, and lets clients browse and restore any past state without ever rewriting history.

## ADDED Requirements

### Requirement: Exactly one commit per mutation
Every successful write tool call SHALL produce exactly one commit containing only the files that call touched. The commit message SHALL be `<tool>: <path>` on the first line (for example `kb_patch_note: Ideas/Idea.md`), followed by an optional client-supplied `summary` paragraph, and trailers `KB-Client: <client name>` (when the MCP client identified itself) and `KB-Instance: <instance id>` (the configured or generated identifier of this server instance). The author SHALL be a dedicated identity (default `Knowledge Base MCP <kb-mcp@localhost>`) so that edits made through the server are distinguishable from human edits in history.

#### Scenario: Patch produces one commit
- **WHEN** a client patches a note
- **THEN** `git log -1` shows one new commit whose diff touches only that note, with the specified message format

#### Scenario: Failed write produces no commit
- **WHEN** a write fails validation
- **THEN** the work tree and history are unchanged

### Requirement: Automatic push with debounce and retry
After a commit, the server SHALL push to the configured remote asynchronously, coalescing commits made within the debounce window (default 5 s) and retrying failures with exponential backoff. Push failures MUST NOT fail the write tool call; they SHALL be visible through `kb_sync_status` as an unpushed-commit count and last error.

#### Scenario: Remote unreachable
- **WHEN** the remote cannot be reached and a client writes a note
- **THEN** the write succeeds, and `kb_sync_status` reports `ahead: 1` with the push error until the next successful push

### Requirement: Automatic pull before writes and periodically
The server owns its clone exclusively: nothing else writes to the work tree. It SHALL fetch and integrate remote changes on a configurable interval (default 60 s) and before each write, unless a successful pull happened within a configurable freshness window (default 5 s). Integration SHALL rebase the server's unpushed commits onto the remote branch so that no merge commits are created. Files changed by the integration SHALL be re-indexed before the pull is reported complete.

#### Scenario: Write lands on latest remote state
- **WHEN** another clone pushed a commit and a client then writes a note
- **THEN** the new commit's parent is the remote's latest commit and no merge commit is created

#### Scenario: Remote change becomes searchable
- **WHEN** a periodic pull brings a commit that adds a note
- **THEN** `kb_search` finds the note without a manual reindex

### Requirement: Conflicts are resolved automatically and nothing is lost
WHEN a rebase stops on a conflicting file, the server SHALL resolve it without human intervention according to the configured strategy and continue. With the default strategy `remote-wins`, the remote version of the file is kept and the server's version is written next to it as `<name>.conflict-<short hash>.md` (excluded from schema validation, included in search) so the losing edit stays visible and can be merged later by a client. With `local-wins` the roles are reversed. Rename/delete conflicts SHALL be resolved by keeping the remote side and saving the local content as the same sibling note. Every automatic resolution SHALL be recorded in `kb_sync_status` (`last_conflicts`) and in the commit message (`resolved-conflict: <path> (<strategy>)`). The server MUST NEVER run force push, history rewriting of pushed commits, or hard resets on the vault, and MUST NEVER leave conflict markers in a committed file.

#### Scenario: Same lines changed on both sides
- **WHEN** the remote and the server both changed the same lines of `Ideas/Idea.md`
- **THEN** after the next pull the work tree holds the remote version at `Ideas/Idea.md`, the server's version at `Ideas/Idea.conflict-<hash>.md`, history is linear, and the push succeeds

#### Scenario: Deleted remotely, edited locally
- **WHEN** the remote deleted a note the server had just patched
- **THEN** the note stays deleted, the patched content is saved as the sibling conflict note, and the event appears in `kb_sync_status`

### Requirement: Only the main branch
The server SHALL commit to and synchronise the configured branch only (default `main`); it SHALL NOT create, switch or delete branches. If the clone is not on that branch at startup, the server MUST refuse to start.

#### Scenario: Wrong branch at startup
- **WHEN** the clone is checked out on `experiment`
- **THEN** the server exits with an error naming the expected branch

### Requirement: Navigating history
The server SHALL let clients move through the vault's Git history without shell access:
- `kb_log(limit, cursor, folder, since)` lists commits on the current branch (hash, date, author, message, changed paths), newest first, with pagination.
- `kb_history(path, limit)` lists commits touching a note, following renames and including commits after which the note no longer exists.
- `kb_ls_tree(revision, folder)` lists notes and folders as they existed at a revision, so a client can discover files that were later deleted or renamed.
- `kb_show_revision(path, revision)` returns a note's content at a revision, including notes deleted since.
- `kb_diff(from, to, path)` returns a unified diff between two revisions, for one note or the whole vault; `to` defaults to the work tree.
Revisions SHALL accept full or abbreviated commit hashes and Git-style relative forms such as `HEAD~3`; unknown revisions fail with code `not_found`.

#### Scenario: Find and read a deleted note
- **WHEN** a note was deleted two commits ago and a client calls `kb_ls_tree("HEAD~2", "Inbox")` then `kb_show_revision("Inbox/Draft.md", "HEAD~2")`
- **THEN** the listing includes the note and the second call returns its content as of that revision

#### Scenario: History of a renamed note
- **WHEN** a note was renamed and then edited
- **THEN** `kb_history` on the new path lists commits from before the rename as well

### Requirement: Restore from history
The server SHALL provide `kb_restore(path, revision)` that writes the content of `path` as of `revision` into the work tree, creating the file if it no longer exists, and commits it as `kb_restore: <path>` with the source revision in the message. History is never rewritten; the restore is a new commit on top.

#### Scenario: Restore an earlier version
- **WHEN** a client restores a note to a revision from yesterday
- **THEN** the work tree matches that revision and a new commit is added on top of history

#### Scenario: Restore a deleted note
- **WHEN** the note does not exist in the work tree but existed at `revision`
- **THEN** the file is recreated with that content, indexed, and committed

### Requirement: Sync status and manual sync
The server SHALL provide `kb_sync_status()` returning branch, ahead/behind counts, state (`ok`, `offline`), times of last successful pull and push, the last error, and `last_conflicts` (path, strategy, resulting sibling note, time) for automatic resolutions since startup; and `kb_sync_now()` triggering an immediate pull and push and returning the resulting status.

#### Scenario: Healthy status
- **WHEN** everything is pushed and the remote is reachable
- **THEN** the status is `ok` with `ahead: 0`, `behind: 0` and an empty `last_conflicts`
