# Spec Delta

## MODIFIED Requirements

### Requirement: Exactly one commit per mutation
Every successful write tool call SHALL produce exactly one commit containing only the files that call touched. The commit message SHALL be `<tool>: <path>` on the first line (for example `kb_patch_note: Ideas/Idea.md`), with the path relative to the repository root and without the repository alias, followed by an optional client-supplied `summary` paragraph, and trailers `KB-Client: <client name>` (when the MCP client identified itself), `KB-User: <account address>` (in multi-user mode) and `KB-Instance: <instance id>` (the configured or generated identifier of this server instance). The author SHALL be a dedicated identity (default `Knowledge Base MCP <kb-mcp@localhost>`), shared by all accounts, so that edits made through the server are distinguishable from human edits in history.

#### Scenario: Patch produces one commit
- **WHEN** a client patches a note
- **THEN** `git log -1` shows one new commit whose diff touches only that note, with the specified message format

#### Scenario: Failed write produces no commit
- **WHEN** a write fails validation
- **THEN** the work tree and history are unchanged

#### Scenario: Acting account in history
- **WHEN** the account `ann@example.com` patches `notes/Ideas/Idea.md` on a multi-user server
- **THEN** the commit in repository `notes` has the subject `kb_patch_note: Ideas/Idea.md`, the shared author and the trailer `KB-User: ann@example.com`

#### Scenario: Classic mode trailers
- **WHEN** a classic-mode client patches a note
- **THEN** the commit has no `KB-User` trailer

### Requirement: Sync status and manual sync
The server SHALL provide `kb_sync_status()` returning branch, ahead/behind counts, state (`ok`, `offline`, `conflict`), times of last successful pull and push, the last error, and `conflicts` (conflicted paths while a merge is in progress); and `kb_sync_now()` triggering an immediate pull and push and returning the resulting status. In multi-user mode both tools SHALL accept an optional repository alias; with it they SHALL act on that repository, and without it on every repository available to the caller, returning one status per repository labelled with its alias. `kb_conflicts` and `kb_reindex` SHALL accept the same optional alias.

#### Scenario: Healthy status
- **WHEN** everything is pushed and the remote is reachable
- **THEN** the status is `ok` with `ahead: 0`, `behind: 0` and an empty `conflicts`

#### Scenario: Status of several repositories
- **WHEN** a multi-user caller with access to repositories `a` (healthy) and `b` (in conflict) calls `kb_sync_status` without an alias
- **THEN** the result contains a status `ok` for `a` and a status `conflict` for `b`

#### Scenario: Sync one repository
- **WHEN** the caller calls `kb_sync_now` with the alias `a`
- **THEN** only `a` is pulled and pushed and only its status is returned
