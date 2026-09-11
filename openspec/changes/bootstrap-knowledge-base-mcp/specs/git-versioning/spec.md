## Purpose

Guarantees that every change to the vault is a reviewable Git commit, keeps the local clone merged with its remote without human intervention, and lets clients browse and restore any past state without ever rewriting history.

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
The server owns its clone exclusively: nothing else writes to the work tree. It SHALL fetch and merge remote changes on a configurable interval (default 60 s) and before each write, unless a successful pull happened within a configurable freshness window (default 5 s). Files changed by the merge SHALL be re-indexed before the pull is reported complete.

#### Scenario: Write lands on latest remote state
- **WHEN** another clone pushed a commit and a client then writes a note
- **THEN** the write's commit descends from the remote's latest commit

#### Scenario: Remote change becomes searchable
- **WHEN** a periodic pull brings a commit that adds a note
- **THEN** `kb_search` finds the note without a manual reindex

### Requirement: Conflicts are committed as Git leaves them
WHEN a merge cannot complete cleanly, the server SHALL NOT stop, revert or pick a side. It SHALL stage the work tree exactly as Git left it, including the standard conflict markers (`<<<<<<<`, `=======`, `>>>>>>>`) inside the affected files, and commit the merge with a message `merge: <remote>/<branch> (conflicts: <paths>)`, then push. Conflicted files remain ordinary notes: they are listed, indexed and searchable (so `kb_grep("<<<<<<<")` finds them) and are resolved by a client editing them with the normal write tools. `kb_sync_status` SHALL list every note that still contains conflict markers under `conflicts`. The server MUST NEVER run force push, history rewriting of pushed commits, or hard resets on the vault.

#### Scenario: Same lines changed on both sides
- **WHEN** the remote and the server both changed the same lines of `Ideas/Idea.md`
- **THEN** after the next pull the file contains both versions between conflict markers, a merge commit records it, the push succeeds, and `kb_sync_status.conflicts` includes `Ideas/Idea.md`

#### Scenario: Client resolves the conflict
- **WHEN** a client replaces the conflicted note with content that has no markers
- **THEN** the write commits normally and the path disappears from `kb_sync_status.conflicts`

#### Scenario: Deleted remotely, edited locally
- **WHEN** the remote deleted a note the server had just patched
- **THEN** the merge is committed with the file kept as Git leaves it for a modify/delete conflict, and the path is reported under `conflicts`

### Requirement: Conflicts are handed to the client that is writing
WHEN the pull performed before a write produces a conflict in the very note being written, the server MUST NOT apply the write. The call SHALL fail with code `merge_conflict` and return everything the client needs to resolve it itself: the note's current content with markers, the two sides separately (`ours` = the server's version before the merge, `theirs` = the remote version), the new `etag`, and the list of any other notes that conflicted in the same merge. A subsequent write with the new `etag` and marker-free content resolves the conflict. The same payload (current content, `etag`) SHALL accompany the `conflict` error raised by a stale `etag`, so a client can merge its intended change into the newer version instead of overwriting it. WHEN a client writes content that still contains conflict markers, the write succeeds and the result carries the warning `conflict_markers_present`.

#### Scenario: Conflict on the note being saved
- **WHEN** a client patches `Ideas/Idea.md`, and the pull before that write conflicts on the same note
- **THEN** the patch is not applied, the result has code `merge_conflict` with `content`, `ours`, `theirs` and `etag`, and the merge commit with markers is already pushed

#### Scenario: Client resolves and saves
- **WHEN** the client then calls `kb_replace_note` with merged content and the returned `etag`
- **THEN** the write commits, the note has no markers, and it disappears from `kb_sync_status.conflicts`

#### Scenario: Stale etag carries the current version
- **WHEN** a write fails with `conflict` because the `etag` is stale
- **THEN** the error includes the current content and `etag` so the client can re-apply its change on top

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
The server SHALL provide `kb_sync_status()` returning branch, ahead/behind counts, state (`ok`, `offline`), times of last successful pull and push, the last error, and `conflicts` (paths of notes that currently contain conflict markers); and `kb_sync_now()` triggering an immediate pull and push and returning the resulting status.

#### Scenario: Healthy status
- **WHEN** everything is pushed and the remote is reachable
- **THEN** the status is `ok` with `ahead: 0`, `behind: 0` and an empty `conflicts`
