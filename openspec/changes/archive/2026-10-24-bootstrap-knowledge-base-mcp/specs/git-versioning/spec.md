## Purpose

Guarantees that every change to the vault is a reviewable Git commit, keeps the local clone merged with its remote, hands unresolvable merges to the client that has the context to resolve them, and lets clients browse and restore any past state without ever rewriting history.

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
After a commit, the server SHALL push to the configured remote asynchronously, coalescing commits made within the debounce window (default 5 s) and retrying failures with exponential backoff. WHEN the push is rejected because the remote moved on, the server SHALL pull (merge) and push again, entering the `conflict` state if that merge conflicts. Push failures MUST NOT fail the write tool call; they SHALL be visible through `kb_sync_status` as an unpushed-commit count and last error.

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

### Requirement: An unresolved merge stops writes and is handed to the client
WHEN a merge cannot complete cleanly, the server SHALL keep the merge in progress locally: nothing is committed, nothing is pushed, and Git's conflict state (work tree with markers, index stages) is preserved. The server enters the `conflict` state. In that state every write tool and `kb_sync_now` MUST fail with code `merge_conflict`, and read tools continue to work. The `merge_conflict` error and the `kb_conflicts()` tool SHALL return, for every conflicted path: the kind of conflict (`content`, `modify/delete`, `delete/modify`, `add/add`), the content with markers, and the three sides separately (`base`, `ours` = the server's side, `theirs` = the remote side; absent when that side deleted the file), plus the remote commits being merged. The state is exposed by `kb_sync_status` (`state: conflict`, `conflicts: [paths]`) and persists across restarts until resolved.

#### Scenario: Conflict discovered before a write
- **WHEN** the pull before `kb_patch_note("Ideas/Idea.md")` conflicts on `Ideas/Idea.md`
- **THEN** the patch is not applied, the call fails with `merge_conflict` carrying `base`, `ours`, `theirs` and the marked content for that path, and nothing has been committed or pushed

#### Scenario: Conflict discovered by the periodic pull
- **WHEN** a periodic pull conflicts on two notes while no client is active
- **THEN** the next write from any client fails with `merge_conflict` listing both notes, and `kb_sync_status` reports `state: conflict`

#### Scenario: Reads during a conflict
- **WHEN** a client calls `kb_get_note` on a conflicted note
- **THEN** it returns the content with markers plus a `conflict` object with the three sides; other notes read normally

### Requirement: The client resolves the merge
The server SHALL provide `kb_resolve_conflict(resolutions)` where each resolution names a conflicted path and either supplies the final `content`, or `take: ours | theirs`, or `delete: true`. The server SHALL validate that the supplied content contains no conflict markers (otherwise fail with `conflict_markers_present` and apply nothing). WHEN every conflicted path has a resolution, the server SHALL stage them, commit the merge with the message `merge: <remote>/<branch> (resolved: <paths>)` and the usual trailers (including `KB-Client`), re-index the changed files, leave the `conflict` state, and push. Partial resolutions SHALL be accepted and remembered until the set is complete, so a client may resolve files one call at a time.

#### Scenario: Full resolution in one call
- **WHEN** a client sends resolutions for all conflicted paths with marker-free content
- **THEN** one merge commit is created, the push succeeds, `kb_sync_status` returns to `ok`, and the resolved notes are searchable

#### Scenario: Shorthand resolution
- **WHEN** a client resolves a path with `take: theirs`
- **THEN** the remote version is used for that path and no content needs to be sent

#### Scenario: Markers left in content
- **WHEN** a resolution's content still contains `<<<<<<<`
- **THEN** the call fails with `conflict_markers_present` and the merge stays in progress

#### Scenario: Partial resolution
- **WHEN** two notes conflict and the client resolves only one
- **THEN** the call succeeds, `kb_conflicts` lists the remaining note, and writes stay blocked until it is resolved too

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
The server SHALL provide `kb_sync_status()` returning branch, ahead/behind counts, state (`ok`, `offline`, `conflict`), times of last successful pull and push, the last error, and `conflicts` (conflicted paths while a merge is in progress); and `kb_sync_now()` triggering an immediate pull and push and returning the resulting status.

#### Scenario: Healthy status
- **WHEN** everything is pushed and the remote is reachable
- **THEN** the status is `ok` with `ahead: 0`, `behind: 0` and an empty `conflicts`
