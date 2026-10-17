# Spec Delta

## Purpose

Defines who may use a multi-user server: how the mode is switched on, how accounts exist, how a person proves an email address with a one-time code, and how every token is tied to one account.

## ADDED Requirements

### Requirement: Multi-user mode is opt-in
The server SHALL run in multi-user mode only when the HTTP transport is enabled and an administrator address is configured (`KB_ADMIN_EMAIL`). Otherwise it SHALL run in classic mode with unchanged behavior. In multi-user mode the server MUST refuse to start, naming the missing setting, unless a mail API key (`KB_RESEND_API_KEY`), a sender address (`KB_MAIL_FROM`) and a valid secret key (`KB_SECRET_KEY`) are configured.

#### Scenario: Classic mode by default
- **WHEN** the server starts with an HTTP listen address and no administrator address
- **THEN** it serves one vault with unprefixed paths, the static token and the password sign-in page, as before this change

#### Scenario: stdio is always classic
- **WHEN** the server starts without an HTTP listen address and an administrator address is set
- **THEN** it runs in classic mode over stdio and logs that the administrator address is ignored

#### Scenario: Incomplete multi-user configuration
- **WHEN** an administrator address is set and the mail API key is missing
- **THEN** the server exits with a non-zero status and an error naming `KB_RESEND_API_KEY`

### Requirement: Accounts are created by the administrator
An account SHALL be identified by an email address, compared case-insensitively, and SHALL be either active or disabled. Accounts SHALL be created only by the administrator; the server MUST NOT create an account because someone tried to sign in. At every start the configured administrator address SHALL exist as an active administrator account.

#### Scenario: First start
- **WHEN** the server starts in multi-user mode with an empty state
- **THEN** exactly one account exists, it is the configured administrator address, and it is active

#### Scenario: Sign-in attempt with an unknown address
- **WHEN** someone submits an address that has no account on the sign-in page
- **THEN** no account is created and no mail is sent

#### Scenario: Address case
- **WHEN** an account exists for `ann@example.com` and a person signs in as `Ann@Example.com`
- **THEN** the same account is used

### Requirement: Sign-in with an emailed one-time code
In multi-user mode the sign-in page SHALL ask for an email address and then for a 6-digit code sent to it. A code SHALL be random, valid for 10 minutes, usable once, and bound to the sign-in request that asked for it. A correct code for an active account completes the sign-in; no password exists. The email SHALL contain the code and its lifetime and MUST NOT contain a link.

#### Scenario: Successful sign-in
- **WHEN** an active account's address is submitted and the code from the email is entered within 10 minutes
- **THEN** the authorization completes and the client receives tokens

#### Scenario: Expired code
- **WHEN** a code is entered 11 minutes after it was sent
- **THEN** sign-in fails and no token is issued

#### Scenario: Code reuse
- **WHEN** a code that already completed a sign-in is entered again
- **THEN** sign-in fails

#### Scenario: Code from another sign-in request
- **WHEN** a code issued for one sign-in request is entered in a different sign-in request for the same address
- **THEN** sign-in fails

### Requirement: Code guessing is bounded
A code SHALL be invalidated after 5 wrong attempts. Requesting a new code SHALL invalidate the previous one for that sign-in request. Wrong attempts MUST be answered identically whether the code was wrong, expired, exhausted or never issued.

#### Scenario: Five wrong attempts
- **WHEN** five wrong codes are entered for a sign-in request
- **THEN** the sixth attempt fails even with the correct code, and a new code must be requested

#### Scenario: New code replaces the old one
- **WHEN** a second code is requested for the same sign-in request
- **THEN** the first code no longer works

### Requirement: Sign-in does not reveal accounts
The sign-in page MUST respond the same way, with no observable difference in content or status, for an address with an active account, a disabled account and no account. Mail delivery failures MUST NOT be shown on the page.

#### Scenario: Unknown address
- **WHEN** an address without an account is submitted
- **THEN** the page shows the same code entry form as for a known address

#### Scenario: Mail provider failure
- **WHEN** the mail provider rejects the message for a known address
- **THEN** the page shows the same code entry form and the failure is logged without the code

### Requirement: Code requests are rate limited
The server SHALL limit how often codes are sent: at most one per address per minute, a bounded number per address per hour, a bounded number per client network address per hour, and a bounded total per hour. A request over a limit SHALL send no mail and SHALL be answered like any other request.

#### Scenario: Repeated request for one address
- **WHEN** a code is requested twice for the same address within one minute
- **THEN** only one email is sent

### Requirement: Tokens belong to an account
Every access and refresh token issued in multi-user mode SHALL belong to the account that signed in, and every request made with it SHALL act as that account. Disabling or deleting an account, or revoking its sessions, MUST make all its tokens stop working immediately. Tokens issued before the switch to multi-user mode MUST NOT be accepted.

#### Scenario: Disabled account
- **WHEN** the administrator disables an account that holds a valid access token
- **THEN** the next request with that token is answered `401`

#### Scenario: Refresh keeps the account
- **WHEN** a client refreshes its token
- **THEN** the new tokens belong to the same account

#### Scenario: Token from before the switch
- **WHEN** a deployment switches to multi-user mode and a client presents a token issued in classic mode
- **THEN** the request is answered `401`, and the client can sign in again without registering again

### Requirement: Static token acts as the administrator
In multi-user mode a request carrying the configured static token (`KB_HTTP_TOKEN`) SHALL act as the administrator account, with that account's access rules and nothing more. The token SHALL remain optional in multi-user mode.

#### Scenario: Recovery access during a mail outage
- **WHEN** mail cannot be delivered and a client sends the static token
- **THEN** its calls are served as the administrator account and see exactly what that account's rules allow

### Requirement: Sign-in secrets stay out of logs and storage
One-time codes MUST NOT be written to logs or to disk. Only hashes of access and refresh tokens SHALL be stored. Logs MAY contain the account address and the outcome of a sign-in.

#### Scenario: Debug logging during sign-in
- **WHEN** log level is `debug` and a person signs in
- **THEN** neither the code nor any token value appears in the log output or in the state file
