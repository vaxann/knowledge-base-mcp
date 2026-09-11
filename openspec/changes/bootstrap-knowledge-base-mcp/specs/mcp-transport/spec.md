## Purpose

Defines how clients connect to the server and how the server is run: protocol surface, the stdio transport, container deployment, concurrency, observability and operational tools.

## ADDED Requirements

### Requirement: MCP protocol compliance
The server SHALL implement the current Model Context Protocol specification for tools and resources, including capability negotiation, `tools/list` with JSON Schema input definitions and English descriptions, structured tool results, and `resources/list`, `resources/read` and resource templates. Tool failures SHALL be returned as tool errors carrying a stable machine-readable `code` and a human-readable `message`.

#### Scenario: Tool discovery
- **WHEN** a client calls `tools/list`
- **THEN** every `kb_*` tool is returned with a schema describing all parameters and their defaults

#### Scenario: Error shape
- **WHEN** a tool fails
- **THEN** the result is marked as an error and contains `code` (for example `not_found`) and `message`

### Requirement: stdio is the only transport
The server SHALL speak MCP over standard input and output and MUST write logs only to standard error so the protocol stream is never corrupted. No network listener is opened. Each client runs its own server instance next to it; instances share state only through the Git remote.

#### Scenario: Launched by a client
- **WHEN** an MCP client launches the binary (or its container) as a subprocess
- **THEN** initialisation completes and tools are usable, with no non-protocol bytes on stdout

#### Scenario: No listening sockets
- **WHEN** the server is running
- **THEN** it holds no listening TCP or Unix sockets

### Requirement: Runs as a container
The project SHALL publish a container image containing the server binary and the `git` CLI. On start inside a container the server SHALL clone the configured remote into a mounted volume if the vault path is empty, and reuse the existing clone otherwise. Git credentials SHALL be provided by mounting an SSH key (read-only) or by an HTTPS token in an environment variable consumed only by Git; the server MUST NOT persist credentials anywhere else. The container SHALL be usable directly as an MCP command (`docker run -i ...`).

#### Scenario: First run with an empty volume
- **WHEN** the container starts with `KB_GIT_REMOTE` set, an empty volume at `KB_VAULT_PATH` and a mounted SSH key
- **THEN** the remote is cloned, the index is built, and the server answers `initialize` on stdio

#### Scenario: Restart with an existing volume
- **WHEN** the container restarts with a populated volume
- **THEN** no clone happens, the server pulls, refreshes the index incrementally and starts

### Requirement: Concurrency model
Read tools SHALL execute concurrently. Write tools and sync operations SHALL be serialised by a vault-level lock so that at most one mutation is in progress; queued writes SHALL execute in arrival order.

#### Scenario: Two calls write at once
- **WHEN** a client issues two patches to different notes simultaneously
- **THEN** both calls succeed and history shows two commits, one per note

### Requirement: Operational tools
The server SHALL provide `kb_info()` returning version, vault path (as configured), branch, note count, index freshness, sync state and the instance identifier used in commit trailers.

#### Scenario: Info call
- **WHEN** a client calls `kb_info`
- **THEN** all fields above are present and the note count equals the number of visible notes

### Requirement: Structured logging without sensitive content
The server SHALL emit structured logs with configurable level. Logs MUST NOT include note bodies, frontmatter values, or Git credentials; they MAY include note paths, tool names and commit hashes.

#### Scenario: Debug logging during a write
- **WHEN** log level is `debug` and a client replaces a note containing a marker string
- **THEN** the marker string never appears in the log output

### Requirement: Stable tool naming
All tools SHALL be prefixed `kb_` and their names, parameters and error codes SHALL be documented; removing or renaming a tool is a breaking change that requires a major version bump.

#### Scenario: Naming check
- **WHEN** `tools/list` is called
- **THEN** every tool name starts with `kb_`
