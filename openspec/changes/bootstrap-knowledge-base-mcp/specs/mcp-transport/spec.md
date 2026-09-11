## Purpose

Defines how clients connect to the server: protocol surface, transports, authentication, concurrency, observability and operational tools.

## ADDED Requirements

### Requirement: MCP protocol compliance
The server SHALL implement the current Model Context Protocol specification for tools and resources, including capability negotiation, `tools/list` with JSON Schema input definitions and English descriptions, structured tool results, and `resources/list`, `resources/read` and resource templates. Tool failures SHALL be returned as tool errors carrying a stable machine-readable `code` and a human-readable `message`.

#### Scenario: Tool discovery
- **WHEN** a client calls `tools/list`
- **THEN** every `kb_*` tool is returned with a schema describing all parameters and their defaults

#### Scenario: Error shape
- **WHEN** a tool fails
- **THEN** the result is marked as an error and contains `code` (for example `not_found`) and `message`

### Requirement: stdio transport
WHEN started without an HTTP listen address, the server SHALL speak MCP over standard input and output, and MUST write logs only to standard error so the protocol stream is never corrupted.

#### Scenario: Launched by a desktop client
- **WHEN** an MCP client launches the binary as a subprocess
- **THEN** initialisation completes and tools are usable, with no non-protocol bytes on stdout

### Requirement: Streamable HTTP transport with bearer authentication
WHEN an HTTP listen address is configured, the server SHALL serve MCP over streamable HTTP. Requests MUST carry `Authorization: Bearer <token>` matching the configured token, compared in constant time; otherwise the server responds `401`. The server MUST refuse to start when the address is not loopback and no token is configured. Multiple clients SHALL be served concurrently. Documentation SHALL state that TLS is expected from a reverse proxy or private network.

#### Scenario: Missing token
- **WHEN** a request arrives without a valid bearer token
- **THEN** the response is `401` and no tool executes

#### Scenario: Unsafe binding refused
- **WHEN** the listen address is `0.0.0.0:8765` and no token is set
- **THEN** the server exits with an error explaining that a token is required

### Requirement: Concurrency model
Read tools SHALL execute concurrently. Write tools and sync operations SHALL be serialised by a vault-level lock so that at most one mutation is in progress; queued writes SHALL execute in arrival order.

#### Scenario: Two clients write at once
- **WHEN** two clients patch different notes simultaneously
- **THEN** both calls succeed and history shows two commits, one per note

### Requirement: Operational tools and health
The server SHALL provide `kb_info()` returning version, vault path (as configured), branch, note count, index freshness and sync state, and, on the HTTP transport, an unauthenticated `GET /healthz` returning `200` when the server can read the vault.

#### Scenario: Health check
- **WHEN** a monitor calls `/healthz`
- **THEN** it receives `200` with a small JSON body including `status: ok`

### Requirement: Structured logging without sensitive content
The server SHALL emit structured logs with configurable level. Logs MUST NOT include note bodies, frontmatter values, or the bearer token; they MAY include note paths and tool names.

#### Scenario: Debug logging during a write
- **WHEN** log level is `debug` and a client replaces a note containing a marker string
- **THEN** the marker string never appears in the log output

### Requirement: Stable tool naming
All tools SHALL be prefixed `kb_` and their names, parameters and error codes SHALL be documented; removing or renaming a tool is a breaking change that requires a major version bump.

#### Scenario: Naming check
- **WHEN** `tools/list` is called
- **THEN** every tool name starts with `kb_`
