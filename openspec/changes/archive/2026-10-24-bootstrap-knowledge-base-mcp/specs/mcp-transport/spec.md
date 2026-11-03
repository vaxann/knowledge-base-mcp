## Purpose

Defines how clients connect to the server and how the server is run: protocol surface, stdio and token-protected HTTP transports, container deployment, concurrency, observability and operational tools.

## ADDED Requirements

### Requirement: MCP protocol compliance
The server SHALL implement the current Model Context Protocol specification for tools and resources, including capability negotiation, `tools/list` with JSON Schema input definitions and English descriptions, structured tool results, and `resources/list`, `resources/read` and resource templates. Tool failures SHALL be returned as tool errors carrying a stable machine-readable `code` and a human-readable `message`.

#### Scenario: Tool discovery
- **WHEN** a client calls `tools/list`
- **THEN** every `kb_*` tool is returned with a schema describing all parameters and their defaults

#### Scenario: Error shape
- **WHEN** a tool fails
- **THEN** the result is marked as an error and contains `code` (for example `not_found`) and `message`

### Requirement: stdio transport by default
WHEN no HTTP listen address is configured, the server SHALL speak MCP over standard input and output and MUST write logs only to standard error so the protocol stream is never corrupted. No network listener is opened in this mode.

#### Scenario: Launched by a client
- **WHEN** an MCP client launches the binary (or its container) as a subprocess
- **THEN** initialisation completes and tools are usable, with no non-protocol bytes on stdout

#### Scenario: No listening sockets in stdio mode
- **WHEN** the server runs without an HTTP listen address
- **THEN** it holds no listening TCP sockets

### Requirement: Streamable HTTP transport with bearer authentication
WHEN an HTTP listen address is configured (`KB_HTTP_LISTEN`), the server SHALL serve MCP over streamable HTTP at `/mcp` and an unauthenticated `GET /healthz`. Every request to `/mcp` MUST carry `Authorization: Bearer <token>` equal to the configured token (compared in constant time); otherwise the server responds `401` with a `WWW-Authenticate` header and executes nothing. The server MUST refuse to start when the address is not loopback and no token is configured. Several clients SHALL be served concurrently by one process, sharing the vault lock. TLS MAY be served directly from a configured certificate and key; otherwise a reverse proxy or private network is expected in front of the port, and the documentation SHALL say so.

#### Scenario: Missing or wrong token
- **WHEN** a request to `/mcp` arrives without a valid bearer token
- **THEN** the response is `401` and no tool executes

#### Scenario: Unsafe binding refused
- **WHEN** the listen address is `0.0.0.0:8765` and no token is set
- **THEN** the server exits with an error explaining that a token is required

#### Scenario: Concurrent remote clients
- **WHEN** three clients with the token write notes at the same time
- **THEN** every write succeeds, history shows one commit per write, and each commit carries the client's `KB-Client` trailer

#### Scenario: Health check
- **WHEN** a monitor calls `/healthz` without a token
- **THEN** it receives `200` with `{"status":"ok"}`

### Requirement: OAuth sign-in for apps that cannot send a static token
WHEN the HTTP transport is enabled, the server SHALL also act as an OAuth 2.1 authorization server for its own `/mcp` resource: it SHALL publish RFC 9728 protected-resource metadata (advertised in the `WWW-Authenticate` header of every 401) and RFC 8414 authorization-server metadata, accept RFC 7591 dynamic client registration with `https` redirect URIs (plus `http://localhost` for development), run the authorization code grant with mandatory PKCE `S256`, show a sign-in page where a person enters the configured password, and issue access tokens (24 h) and rotating refresh tokens (90 days). Access tokens SHALL be accepted by `/mcp` exactly like the static token. Registered clients and token hashes SHALL survive restarts. Wrong passwords MUST be delayed and MUST NOT reveal anything. Authorization codes and sign-in requests MUST be single use and short-lived.

#### Scenario: Claude app connector
- **WHEN** a Claude app adds `https://kb.example.com/mcp` as a connector with sign-in required and no client credentials
- **THEN** it discovers the metadata, registers itself, sends the user to the sign-in page, and after the correct password obtains tokens that make `/mcp` calls succeed

#### Scenario: Wrong password
- **WHEN** a wrong password is submitted on the sign-in page
- **THEN** the response is `401` with the form re-rendered, no code is issued, and the request is delayed by at least one second

#### Scenario: Refresh rotation
- **WHEN** a client refreshes its token
- **THEN** it receives a new access and refresh token, and the previous refresh token and its access token stop working

#### Scenario: Restart
- **WHEN** the server restarts
- **THEN** previously issued, unexpired tokens keep working and registered clients remain known

### Requirement: One server process per clone
The server SHALL take an exclusive lock on the index directory at startup and MUST refuse to start, with a message naming the lock, when another instance already holds it, so two processes never operate on the same clone and index.

#### Scenario: Second instance on the same volume
- **WHEN** a second server starts with the same `KB_INDEX_DIR`
- **THEN** it exits with an error naming the lock file and the running instance is unaffected

### Requirement: Runs as a container
The project SHALL publish a container image containing the server binary and the `git` CLI. On start inside a container the server SHALL clone the configured remote into a mounted volume if the vault path is empty, and reuse the existing clone otherwise. A Compose file SHALL be provided for a long-running instance serving the HTTP transport with a token, with a health check and automatic restart. Both credential methods SHALL be supported and documented: a read-only mounted SSH key, and an HTTPS token in an environment variable consumed only by Git; the server MUST NOT persist credentials anywhere else. The image SHALL be published for `linux/amd64` and `linux/arm64`. The container SHALL be usable directly as an MCP command (`docker run -i ...`).

#### Scenario: First run with an empty volume
- **WHEN** the container starts with `KB_GIT_REMOTE` set, an empty volume at `KB_VAULT_PATH` and a mounted SSH key
- **THEN** the remote is cloned, the index is built, and the server answers `initialize` on stdio

#### Scenario: First run with an HTTPS token
- **WHEN** the container starts with an HTTPS remote and a token environment variable instead of an SSH key
- **THEN** the clone and later pushes succeed and the token is never written to the volume or logs

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
