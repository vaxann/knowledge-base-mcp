## Purpose

Guarantees that every change to the vault is a reviewable Git commit, keeps the local clone synchronised with its remote, and lets clients browse and restore any past state without ever rewriting history.

## ADDED Requirements

### Requirement: Exactly one commit per mutation
Every successful write tool call SHALL produce exactly one commit containing only the files that call touched. The commit message SHALL be `<tool>: <path>` on the first line (for example `kb_patch_note: Ideas/Idea.md`), followed by an optional client-supplied `summary` paragraph, and a trailer `KB-Client: <client name>` when the MCP client identified itself. The author SHALL be the configured name and email.

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
The server SHALL fetch and integrate remote changes (fast-forward, or rebase of its own unpushed commits) before each write and on a configurable interval (default 60 s), and SHALL notify the index of files changed by the integration.

#### Scenario: Write lands on latest remote state
- **WHEN** another clone pushed a commit and a client then writes a note
- **THEN** the new commit's parent is the remote's latest commit and no merge commit is created

### Requirement: Conflicts are surfaced, never forced
WHEN an integration cannot complete cleanly, the server SHALL abort the rebase, keep local commits intact, enter the `conflict` state, and reject subsequent writes with code `sync_conflict` while continuing to serve reads. The server MUST NEVER run force push, history rewriting or hard resets on the vault.

#### Scenario: Divergent history
- **WHEN** the remote and the local clone both changed the same lines of a note
- **THEN** `kb_sync_status` reports state `conflict`, reads still work, and writes fail with `sync_conflict` until a human resolves the clone

### Requirement: Foreign uncommitted changes
WHEN the work tree contains uncommitted changes not made by the server, the server SHALL still commit only its own touched files, SHALL report the dirty state in `kb_sync_status`, and, only when `autocommit_external` is enabled, SHALL first commit those foreign changes with the message `external: changes made outside the server`.

#### Scenario: Dirty tree with autocommit disabled
- **WHEN** a desktop editor left an unsaved-to-git change and a client writes another note
- **THEN** the server's commit contains only its note and `kb_sync_status` shows `dirty: true`

#### Scenario: Dirty tree with autocommit enabled
- **WHEN** `autocommit_external` is true in the same situation
- **THEN** two commits result: the external one first, then the server's

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
The server SHALL provide `kb_sync_status()` returning branch, ahead/behind counts, `dirty`, state (`ok`, `offline`, `conflict`), times of last successful pull and push, and the last error; and `kb_sync_now()` triggering an immediate pull and push and returning the resulting status.

#### Scenario: Healthy status
- **WHEN** everything is pushed and the remote is reachable
- **THEN** the status is `ok` with `ahead: 0`, `behind: 0`, `dirty: false`
