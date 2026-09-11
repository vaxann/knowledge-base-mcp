## Purpose

Keeps note cards consistent: frontmatter must always be valid YAML that Obsidian and Dataview can read, and folders can declare a schema that every note inside must satisfy.

## ADDED Requirements

### Requirement: Frontmatter must be valid YAML on every write
Every write tool SHALL parse the resulting frontmatter before committing. If it is not a valid YAML mapping (including the common mistakes of an unquoted value containing `: ` or a stray tab), the call MUST fail with code `invalid_frontmatter` and a message pointing at the line, and nothing is written. When the server serialises frontmatter itself (`kb_create_note` with `frontmatter`, `set_frontmatter`), it MUST quote scalars that would otherwise be ambiguous, so the output round-trips through YAML unchanged.

#### Scenario: Unquoted colon
- **WHEN** a client writes content whose header contains `phone: +381 60: 000`
- **THEN** the call fails with `invalid_frontmatter` naming the line

#### Scenario: Server-side quoting
- **WHEN** a client calls `set_frontmatter({url: "https://example.com/a: b"})`
- **THEN** the written value is quoted and reading the note back yields the identical string

### Requirement: Folder schemas stored in the vault
A folder MAY contain a schema file (default name `_schema.yaml`, configurable) that applies to every note in that folder and its subfolders; the nearest schema file wins. The schema SHALL support: `required` (list of field names), `properties` with per-field `type` (`string`, `number`, `boolean`, `date`, `list`, `link`), `enum`, `pattern`, and `items` for lists, plus `additionalProperties` (default `true`) and `filename_pattern` (regular expression the note's file stem must match). Schema files are ordinary vault files: they are versioned, readable through `kb_get_note`-style tools and editable through the write tools, and MUST be re-loaded when they change.

#### Scenario: Nearest schema applies
- **WHEN** `Contacts/_schema.yaml` and `Contacts/Clients/_schema.yaml` both exist
- **THEN** a note in `Contacts/Clients/` is validated only against the latter

#### Scenario: Schema edited through the server
- **WHEN** a client adds a value to an `enum` in a schema file via `kb_patch_note`
- **THEN** the next write of a note in that folder is validated against the updated enum

### Requirement: Writes are validated against the folder schema
When a schema applies to the target path, `kb_create_note`, `kb_replace_note`, `kb_patch_note` and `kb_move_note` (for the destination) SHALL validate the resulting frontmatter and file name. In `strict` mode (default) a violation fails the call with code `schema_violation` and a list of problems (field, rule, actual value) and nothing is written; in `warn` mode the write proceeds and the problems are returned as `warnings`. Sibling conflict notes produced by sync are exempt.

#### Scenario: Missing required field
- **WHEN** the schema requires `type` and a client creates a note without it
- **THEN** the call fails with `schema_violation` listing `type: required`

#### Scenario: Value outside the enum
- **WHEN** the schema restricts `status` to `active | archive` and a patch sets `status: done`
- **THEN** the call fails with `schema_violation` and the note is unchanged

#### Scenario: Warn mode
- **WHEN** the server runs with `schema.mode: warn` and the same patch is applied
- **THEN** the write succeeds and the result carries a `warnings` entry for `status`

### Requirement: Audit existing notes
The server SHALL provide `kb_validate(folder, limit)` that checks every visible note under `folder` (default: whole vault) for YAML validity and schema conformance without modifying anything, returning per-note problems so a client can fix them with the write tools.

#### Scenario: Vault audit
- **WHEN** a client calls `kb_validate("Contacts")`
- **THEN** every note under `Contacts/` with invalid YAML or a schema violation is listed with its problems, and conformant notes are not
