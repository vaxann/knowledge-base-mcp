# Spec Delta

## MODIFIED Requirements

### Requirement: All paths are vault-relative and sandboxed
Every path accepted or returned by a tool SHALL use forward slashes and SHALL be relative to the vault root in classic mode; in multi-user mode it SHALL be a repository alias followed by a path relative to that repository's root, as defined by the `repositories` capability. The server MUST reject absolute paths, paths containing `..` segments, and paths that resolve (including through symlinks) outside the root of the vault or repository they address, returning error code `invalid_path`.

#### Scenario: Traversal attempt
- **WHEN** a tool receives the path `../outside.md` or `/etc/passwd`
- **THEN** the call fails with code `invalid_path` and no filesystem access outside the vault occurs

#### Scenario: Symlink escape
- **WHEN** a path inside the vault is a symlink that resolves outside the vault root
- **THEN** the call fails with code `invalid_path`

#### Scenario: Escape from one repository into another
- **WHEN** a multi-user client reads `a/../b/note.md`
- **THEN** the call fails with code `invalid_path` and repository `b` is not accessed

### Requirement: Credentials never live in tracked files or logs
In classic mode the server SHALL hold no credentials of its own: Git authentication comes from the host or container Git setup (SSH key, credential helper, or an HTTPS token consumed by Git through an environment variable). In multi-user mode the server SHALL additionally keep the access tokens and SSH private keys of registered repositories, at rest only in its state file and only encrypted; a decrypted SSH key MAY exist as a temporary file outside the data volume while the server runs, as defined by the `repositories` capability. The server MUST NOT write credentials to the vault, any repository, the index, its configuration, or logs. The project repository SHALL contain only an example configuration with placeholder values.

#### Scenario: HTTPS token in environment
- **WHEN** a Git token is provided through the environment and logging is at debug level
- **THEN** the token value never appears in any log line, and no file under the vault or index directory contains it

#### Scenario: Credential submitted in the admin panel
- **WHEN** a repository is added with a token or an SSH private key and logging is at debug level
- **THEN** the credential never appears in any log line, in the repository clone, or in clear text in the state file
