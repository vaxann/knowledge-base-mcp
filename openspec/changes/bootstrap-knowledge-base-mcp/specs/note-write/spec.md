## Purpose

Lets clients create and modify notes safely: validated input, atomic writes, optimistic concurrency, plain rename and delete semantics with Git as the safety net.

## ADDED Requirements

### Requirement: Create a note
The server SHALL provide `kb_create_note(path, content, frontmatter, overwrite)` that creates a Markdown note, creating parent folders as needed. The path MUST end in `.md`. WHEN `frontmatter` is given, it SHALL be serialised as the YAML header in front of `content` (or merged over a header already present in `content`). The server SHALL NOT apply templates; clients wanting a template read it from the vault like any other note and pass the resulting content. The result SHALL include the created path, `etag` and the commit hash.

#### Scenario: New note with frontmatter
- **WHEN** a client creates `Contacts/Alice.md` with content `# Alice` and frontmatter `{role: engineer}`
- **THEN** the file starts with a YAML header containing `role: engineer`, followed by the body, and one commit exists for it

#### Scenario: Note already exists
- **WHEN** the target path exists and `overwrite` is false
- **THEN** the call fails with code `already_exists` and nothing is written

#### Scenario: Wrong extension
- **WHEN** the path does not end in `.md`
- **THEN** the call fails with code `invalid_path`

### Requirement: Replace note content with optimistic concurrency
The server SHALL provide `kb_replace_note(path, content, etag)` that replaces the whole file. WHEN `etag` is provided and does not match the current content hash, the call MUST fail with code `conflict` and MUST NOT modify the file. Frontmatter in `content` MUST be valid YAML or the call fails with `invalid_frontmatter`.

#### Scenario: Matching etag
- **WHEN** the supplied `etag` equals the current hash
- **THEN** the content is replaced, a new `etag` is returned, and one commit exists

#### Scenario: Stale etag
- **WHEN** another client changed the note after the `etag` was obtained
- **THEN** the call fails with code `conflict` and the file is unchanged

### Requirement: Patch a note with atomic operations
The server SHALL provide `kb_patch_note(path, operations, etag)` applying an ordered list of operations atomically (all or none): `append`, `prepend`, `replace_section(heading, content)`, `insert_after_heading(heading, content)`, `set_frontmatter(fields)` (merge, keeping other keys), `remove_frontmatter(keys)`, and `find_replace(find, replace, all)`. A failed operation (for example a missing heading) MUST fail the whole call with code `patch_failed` and leave the file unchanged.

#### Scenario: Frontmatter-only edit
- **WHEN** a client applies `set_frontmatter({status: done})`
- **THEN** the body bytes are identical to before and only the `status` line changed or was added

#### Scenario: Replace a section
- **WHEN** a client applies `replace_section("## Log", "new text")`
- **THEN** the text between that heading and the next heading of the same or higher level is replaced and the heading itself is kept

#### Scenario: Missing heading
- **WHEN** `insert_after_heading` names a heading that does not exist
- **THEN** the call fails with code `patch_failed` and no operation is applied

### Requirement: Move or rename without touching other notes
The server SHALL provide `kb_move_note(from, to)` that renames a note inside the vault as a Git rename. It MUST fail with `already_exists` if `to` exists and with `not_found` if `from` does not. The server SHALL NOT rewrite links in other notes; instead the result SHALL list the notes that linked to the old path (from the backlink graph) so the client can update them with `kb_grep` and `kb_patch_note` if it chooses to.

#### Scenario: Rename with backlinks
- **WHEN** `Old.md` is moved to `Archive/New.md` and two notes link `[[Old]]`
- **THEN** only the rename is committed, and the result lists those two notes as `referencing_notes`

### Requirement: Delete a note permanently
The server SHALL provide `kb_delete_note(path, etag)` that removes the file from the work tree and commits the deletion. There is no trash folder; recovery is done through Git history with `kb_restore`.

#### Scenario: Delete and recover
- **WHEN** a client deletes `Inbox/Draft.md` and later calls `kb_restore("Inbox/Draft.md", "<commit before deletion>")`
- **THEN** after the delete the file is absent, unlisted and unsearchable, and after the restore it is back with its previous content and a new commit

#### Scenario: Stale etag on delete
- **WHEN** `etag` is provided and does not match
- **THEN** the call fails with code `conflict` and the file remains

### Requirement: Writes are atomic on disk and immediately searchable
Every write SHALL be performed by writing a temporary file and renaming it into place, so readers never observe partial content. After a write tool returns successfully, the search index MUST already reflect the change.

#### Scenario: Search right after create
- **WHEN** a note containing the word `zebra` is created and `kb_search("zebra")` is called immediately
- **THEN** the new note is among the results

### Requirement: Read-only mode
WHEN the server runs with `read_only` enabled, write tools SHALL NOT be listed and any attempt to call them MUST fail with code `read_only`.

#### Scenario: Tools list in read-only mode
- **WHEN** a client lists tools on a read-only server
- **THEN** no `kb_create_note`, `kb_replace_note`, `kb_patch_note`, `kb_move_note`, `kb_delete_note` or `kb_restore` appears
