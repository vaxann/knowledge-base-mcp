## Purpose

Defines how the server locates, configures and sandboxes the knowledge base vault on disk, and the note model every other capability relies on.

## ADDED Requirements

### Requirement: Vault location is explicit and validated
The server SHALL take the vault location from the `KB_VAULT_PATH` environment variable or the `vault.path` configuration key, with environment variables taking precedence over the configuration file and the file over built-in defaults. The server MUST refuse to start when the path is missing, unreadable, or not a Git work tree, and MUST print a single actionable error.

#### Scenario: Valid vault path
- **WHEN** the configured path exists and contains a Git work tree
- **THEN** the server starts and reports the vault path and current branch in `kb_info`

#### Scenario: Missing vault path
- **WHEN** no path is configured, or the path does not exist and no remote is configured
- **THEN** the server exits with a non-zero status and an error naming the missing setting

#### Scenario: Path is not a Git repository
- **WHEN** the path exists but is not inside a Git work tree
- **THEN** the server exits with a non-zero status and an error explaining that a Git clone is required

### Requirement: Optional bootstrap clone
WHEN the vault path does not exist AND a Git remote is configured, the server SHALL clone the remote into the path before starting. Credentials MUST come from the host Git configuration (SSH agent, credential helper); the server SHALL NOT accept credentials in its own configuration.

#### Scenario: First start on a new machine
- **WHEN** the path is absent and `KB_GIT_REMOTE` is set
- **THEN** the server clones the remote, then continues startup as if the vault had existed

### Requirement: All paths are vault-relative and sandboxed
Every path accepted or returned by a tool SHALL be relative to the vault root using forward slashes. The server MUST reject absolute paths, paths containing `..` segments, and paths that resolve (including through symlinks) outside the vault root, returning error code `invalid_path`.

#### Scenario: Traversal attempt
- **WHEN** a tool receives the path `../outside.md` or `/etc/passwd`
- **THEN** the call fails with code `invalid_path` and no filesystem access outside the vault occurs

#### Scenario: Symlink escape
- **WHEN** a path inside the vault is a symlink that resolves outside the vault root
- **THEN** the call fails with code `invalid_path`

### Requirement: Excluded paths are invisible
The server SHALL hide, by default, `.git/`, `.obsidian/`, `.trash/` and any other directory whose name starts with a dot, and SHALL support additional user-configured glob patterns. Excluded paths MUST NOT appear in listings, search results or resources and MUST NOT be readable or writable through tools, returning `not_found` when addressed directly.

#### Scenario: Default exclusion
- **WHEN** a client lists the vault root
- **THEN** `.obsidian/` and `.git/` are absent from the result

#### Scenario: User exclusion
- **WHEN** `vault.exclude` contains `Private/**` and a client requests `Private/secret.md`
- **THEN** the call fails with code `not_found`

### Requirement: Note model with order-preserving frontmatter
A note is a UTF-8 Markdown file whose name ends in `.md`. The server SHALL parse an optional leading YAML frontmatter block into a structured map and the remainder as the body. WHEN a note is written back after modifying frontmatter, the server MUST preserve the order of untouched keys, their scalar formatting, comments, and the body byte-for-byte, and MUST preserve the file's existing line-ending style.

#### Scenario: Read frontmatter
- **WHEN** a note starts with a `---` block containing `type: contact` and `tags: [work]`
- **THEN** the returned frontmatter contains `type` = `contact` and `tags` = `["work"]`, and the body excludes the block

#### Scenario: Round-trip after a frontmatter edit
- **WHEN** a client sets one frontmatter key on a note with three existing keys and a comment
- **THEN** the written file differs from the original only in that key's line

#### Scenario: Invalid frontmatter on read
- **WHEN** a note's frontmatter is not valid YAML
- **THEN** the note is still returned with `frontmatter_error` set and an empty frontmatter map, and the raw content is intact

### Requirement: Non-Markdown files are listed but not edited
Files that are not Markdown notes (attachments such as PDF or images) SHALL appear in listings with name, size and media type, but write tools MUST reject them with code `unsupported_file`, and read tools MUST NOT return their bytes as text.

#### Scenario: Listing a folder with attachments
- **WHEN** a client lists a folder containing `note.md` and `scan.pdf`
- **THEN** both entries are returned, with `scan.pdf` marked as `kind: attachment`

#### Scenario: Attempt to edit an attachment
- **WHEN** a client calls a write tool on `scan.pdf`
- **THEN** the call fails with code `unsupported_file`

### Requirement: Credentials never live in tracked files or logs
The server SHALL hold no credentials of its own: Git authentication comes from the host or container Git setup (SSH key, credential helper, or an HTTPS token consumed by Git through an environment variable). The server MUST NOT write credentials to the vault, the index, its configuration, or logs. The project repository SHALL contain only an example configuration with placeholder values.

#### Scenario: HTTPS token in environment
- **WHEN** a Git token is provided through the environment and logging is at debug level
- **THEN** the token value never appears in any log line, and no file under the vault or index directory contains it
