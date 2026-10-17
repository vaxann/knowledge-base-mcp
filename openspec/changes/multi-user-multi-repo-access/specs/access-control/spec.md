# Spec Delta

## Purpose

Decides what each account may see and change on a multi-user server: folder-level rules, how they combine, and the guarantee that content outside an account's rules is indistinguishable from content that does not exist.

## ADDED Requirements

### Requirement: Folder-level access rules
An access rule SHALL bind one account to one folder of one repository with a level: `none`, `read` or `write`. `write` includes `read`. The folder MAY be the repository root. At most one rule SHALL exist per account and folder. Rules SHALL survive restarts, and a change to rules SHALL apply to the next request without a restart or a new sign-in.

#### Scenario: Rule on a repository root
- **WHEN** an account has the rule `read` on the root of repository `notes`
- **THEN** it can read every note in `notes` and change none

#### Scenario: Immediate effect
- **WHEN** the administrator changes an account's rule on `notes/Projects` from `read` to `none` while that account holds a valid token
- **THEN** the account's next read under `notes/Projects` fails with `not_found`

### Requirement: Deepest rule wins
The level of an account on a path SHALL be the level of its rule on the longest folder that contains the path, matched on whole path segments. Without any such rule the level SHALL be `none`. A deeper rule MAY lower or raise the level set by a shallower one.

#### Scenario: Closed subfolder inside an open folder
- **WHEN** an account has `write` on `notes/Projects` and `none` on `notes/Projects/Secret`
- **THEN** it can edit `notes/Projects/a.md` and reading `notes/Projects/Secret/b.md` fails with `not_found`

#### Scenario: Open subfolder inside a closed repository
- **WHEN** an account's only rule is `read` on `notes/Shared/Team`
- **THEN** it can read `notes/Shared/Team/x.md` and reading `notes/Shared/y.md` fails with `not_found`

#### Scenario: Segment boundaries
- **WHEN** an account has `read` on `notes/Work`
- **THEN** it cannot read `notes/Workshop/a.md`

#### Scenario: No rules
- **WHEN** an account has no rules
- **THEN** the root listing is empty and every path fails with `not_found`

### Requirement: The administrator has no implicit access to content
The administrator account SHALL be subject to access rules like any other account: without a rule its level is `none`. Requests made with the static token act as the administrator account and SHALL follow the same rules. Administrative powers (managing accounts, repositories and rules, rebuilding an index, triggering a sync from the admin panel) SHALL NOT depend on rules and SHALL NOT reveal note content.

#### Scenario: New repository
- **WHEN** a repository is added and no rules exist for it
- **THEN** no account can see it, the administrator included, and it is absent from every root listing

#### Scenario: Administrator grants itself access
- **WHEN** the administrator sets `read` for its own account on the root of a repository
- **THEN** its next root listing shows that repository and it can read its notes

#### Scenario: Static token
- **WHEN** a client sends the static token and the administrator account has no rules
- **THEN** the root listing is empty and every path fails with `not_found`

### Requirement: Unreadable content does not exist
For a caller whose level on a path is `none`, every operation addressing that path SHALL fail with `not_found`, with the same response as for a path that truly does not exist, whether or not a file is there. This SHALL hold for tools, resources and the file routes alike.

#### Scenario: Existing and missing look the same
- **WHEN** an account without access reads `notes/Private/real.md`, which exists, and `notes/Private/missing.md`, which does not
- **THEN** both calls fail with `not_found` and identical messages apart from the path

#### Scenario: Create in an unreadable folder
- **WHEN** an account without access creates `notes/Private/new.md`
- **THEN** the call fails with `not_found` and nothing is written

#### Scenario: Resource read
- **WHEN** an account without access reads the resource for `notes/Private/real.md`
- **THEN** the resource is reported as not found

### Requirement: Writes need the write level
A mutation SHALL succeed only when the caller's level is `write` on every path it changes. WHEN the caller can read but not write a path, the call SHALL fail with code `forbidden` and change nothing. A move SHALL require `write` on both the source and the destination.

#### Scenario: Read-only folder
- **WHEN** an account with `read` on `notes/Docs` patches `notes/Docs/a.md`
- **THEN** the call fails with `forbidden` and no commit is made

#### Scenario: Move into a read-only folder
- **WHEN** an account with `write` on `notes/Inbox` and `read` on `notes/Docs` moves `notes/Inbox/a.md` to `notes/Docs/a.md`
- **THEN** the call fails with `forbidden`

#### Scenario: Server-wide read-only mode
- **WHEN** the server runs with `read_only` enabled and an account with `write` patches a note
- **THEN** the call fails with `read_only`

### Requirement: Listings show only reachable entries
A listing SHALL contain only entries the caller can read, plus the folders that lead to something the caller can read. Such a pass-through folder SHALL list only its children that are readable or lead further. A repository SHALL appear at the root only when the caller can read something in it.

#### Scenario: Path to a deep grant
- **WHEN** an account's only rule is `read` on `notes/Shared/Team`, and `notes/Shared` also contains `Other/` and `y.md`
- **THEN** listing the root shows `notes`, listing `notes` shows only `Shared`, and listing `notes/Shared` shows only `Team`

#### Scenario: Hidden subfolder
- **WHEN** an account has `read` on `notes/Projects` and `none` on `notes/Projects/Secret`
- **THEN** a recursive listing of `notes/Projects` contains no entry under `Secret` and no `Secret` folder

### Requirement: Search results respect rules
Full-text search, grep, structured query, quick open and context bundles SHALL consider only notes the caller can read. Result counts, offsets and limits SHALL be computed over readable notes only. Tag listings SHALL count only readable notes, so a tag used only in unreadable notes does not appear.

#### Scenario: Match in a hidden note
- **WHEN** the only note matching `zebra` is unreadable for the caller
- **THEN** the search returns no results and a total of zero

#### Scenario: Page size
- **WHEN** 30 notes match, 10 of them unreadable, and the caller asks for 20 results
- **THEN** 20 readable results are returned

#### Scenario: Tag only in hidden notes
- **WHEN** the tag `secret-project` is used only in unreadable notes
- **THEN** the caller's tag listing does not contain it

### Requirement: Links do not reveal hidden notes
A link whose target the caller cannot read SHALL be reported as unresolved. Backlinks and the list of referencing notes returned by a move SHALL omit notes the caller cannot read.

#### Scenario: Link to a hidden note
- **WHEN** a readable note links to an unreadable note
- **THEN** the link appears among the note's links without a resolved path

#### Scenario: Backlink from a hidden note
- **WHEN** an unreadable note links to a readable note
- **THEN** the readable note's backlinks do not include it

### Requirement: History respects rules
History tools SHALL apply the caller's current rules to the paths involved. A commit SHALL be listed only when it touches a readable path, and its list of changed paths SHALL contain readable paths only. WHEN a listed commit also touches unreadable paths, its message SHALL be withheld and the entry marked as partial. A diff of a whole repository SHALL contain readable paths only. History of a renamed note SHALL stop at the point where its earlier path is unreadable.

#### Scenario: Commit in a hidden folder
- **WHEN** a commit changes only unreadable paths
- **THEN** it is absent from the caller's log

#### Scenario: Mixed commit
- **WHEN** one commit changes a readable note and an unreadable note
- **THEN** the log entry lists only the readable note, has no message and is marked partial

#### Scenario: Old revision of a hidden note
- **WHEN** a caller requests a past revision of a note under an unreadable path
- **THEN** the call fails with `not_found`

#### Scenario: Restore
- **WHEN** an account with `read` on a folder restores a note in it
- **THEN** the call fails with `forbidden`

### Requirement: Sync and conflict tools respect rules
Sync status and manual sync SHALL cover only repositories in which the caller can read something. Conflict reports SHALL list only readable conflicted paths and SHALL state how many other conflicted paths exist. Resolving a merge SHALL require `write` on every conflicted path; otherwise it SHALL fail with `forbidden`, saying that an account with wider access must resolve it. Rebuilding an index SHALL be reserved to the administrator, for any repository, and its result SHALL report status only.

#### Scenario: Partial view of a conflict
- **WHEN** a merge conflicts on one readable and one unreadable path
- **THEN** the caller's conflict report shows the readable path and a hidden count of 1, and its attempt to resolve fails with `forbidden`

#### Scenario: Reindex by a regular account
- **WHEN** a non-administrator calls `kb_reindex`
- **THEN** the call fails with `forbidden`

#### Scenario: Reindex without content access
- **WHEN** the administrator, holding no rule on repository `notes`, calls `kb_reindex` for `notes`
- **THEN** the index is rebuilt and the result contains no path, title or note count

#### Scenario: Conflict nobody can resolve
- **WHEN** a merge conflicts on paths that no single account can write
- **THEN** every resolve attempt fails with `forbidden` until the administrator grants an account `write` on all conflicted paths

### Requirement: Signed links carry their issuer's access
Creating a download link SHALL require `read` and creating an upload link SHALL require `write` on the target. A signed link SHALL be bound to the account that created it, and at the moment it is used the server SHALL verify that the account is still active and still holds the required level; otherwise the response SHALL be `403`. Requests to the file routes authenticated by a token instead of a signature SHALL be subject to the same rules as tools.

#### Scenario: Access revoked before use
- **WHEN** an account creates a download link and the administrator removes its access before the link is opened
- **THEN** opening the link returns `403`

#### Scenario: Upload link without write access
- **WHEN** an account with `read` on a folder asks for an upload link into it
- **THEN** the call fails with `forbidden`

### Requirement: Classic mode has one unrestricted caller
In classic mode every authenticated request and every stdio client SHALL have `write` on the whole vault, and no access rules SHALL exist.

#### Scenario: Classic write
- **WHEN** a classic-mode client with the static token patches any note
- **THEN** the write succeeds as before this change
