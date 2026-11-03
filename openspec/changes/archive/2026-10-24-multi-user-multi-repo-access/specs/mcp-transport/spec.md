# Spec Delta

## MODIFIED Requirements

### Requirement: OAuth sign-in for apps that cannot send a static token
WHEN the HTTP transport is enabled, the server SHALL also act as an OAuth 2.1 authorization server for its own `/mcp` resource: it SHALL publish RFC 9728 protected-resource metadata (advertised in the `WWW-Authenticate` header of every 401) and RFC 8414 authorization-server metadata, accept RFC 7591 dynamic client registration with `https` redirect URIs (plus `http://localhost` for development), run the authorization code grant with mandatory PKCE `S256`, show a sign-in page, and issue access tokens (24 h) and rotating refresh tokens (90 days). In classic mode the sign-in page SHALL ask for the configured password. In multi-user mode it SHALL ask for an email address and an emailed one-time code as defined by the `user-accounts` capability, and the configured password SHALL be ignored. Access tokens SHALL be accepted by `/mcp` exactly like the static token, except that in multi-user mode each acts as the account that signed in. Registered clients and token hashes SHALL survive restarts. Wrong passwords and wrong codes MUST be delayed and MUST NOT reveal anything. Authorization codes and sign-in requests MUST be single use and short-lived.

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

#### Scenario: Connector on a multi-user server
- **WHEN** a Claude app adds the connector of a multi-user server and the person enters their account address and the emailed code
- **THEN** the app obtains tokens, and its `/mcp` calls see only what that account may access

#### Scenario: Password on a multi-user server
- **WHEN** a multi-user server has a sign-in password configured
- **THEN** the sign-in page offers no password field and the password grants nothing

### Requirement: Operational tools
The server SHALL provide `kb_info()`. In classic mode it SHALL return version, vault path (as configured), branch, note count, index freshness, sync state and the instance identifier used in commit trailers. In multi-user mode it SHALL return version, instance identifier, the caller's account address and, for each repository the caller can read, its alias, branch, sync state, index freshness and the number of notes the caller can read; it MUST NOT return filesystem paths or remote URLs.

#### Scenario: Info call
- **WHEN** a client calls `kb_info`
- **THEN** all fields above are present and the note count equals the number of visible notes

#### Scenario: Info on a multi-user server
- **WHEN** an account that can read 3 of the 10 notes in repository `notes` and nothing in repository `other` calls `kb_info`
- **THEN** the result lists only `notes` with a note count of 3 and contains no filesystem path
