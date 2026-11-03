# Spec Delta

## Purpose

Gives the administrator of a multi-user server a web interface to manage accounts, repositories and access rules without editing files or restarting the server.

## ADDED Requirements

### Requirement: Admin panel is reserved to the administrator
In multi-user mode the server SHALL serve an admin panel under `/admin`. Every panel page and action SHALL require an administrator session; without one the visitor is sent to the panel sign-in page and nothing else is revealed. In classic mode `/admin` SHALL NOT exist.

#### Scenario: Visitor without a session
- **WHEN** a browser without a session requests `/admin/users`
- **THEN** it is redirected to the panel sign-in page and no account data is in the response

#### Scenario: Regular account
- **WHEN** a non-administrator completes the email code on the panel sign-in page
- **THEN** no session is created and the page answers as it does for a wrong code

#### Scenario: Classic mode
- **WHEN** a classic-mode server receives a request for `/admin`
- **THEN** the response is `404`

### Requirement: Panel sign-in
The panel SHALL sign the administrator in with the same emailed one-time code as the OAuth sign-in, under the same limits. It SHALL also accept the static token as a recovery sign-in when one is configured; a wrong token MUST be delayed and MUST NOT reveal anything. A session SHALL expire after 12 hours and SHALL end on sign-out.

#### Scenario: Email code
- **WHEN** the administrator enters their address and the code from the email
- **THEN** a session starts and the panel opens

#### Scenario: Recovery during a mail outage
- **WHEN** mail cannot be delivered and the administrator submits the static token on the sign-in page
- **THEN** a session starts

#### Scenario: Sign-out
- **WHEN** the administrator signs out and the browser reuses the old session cookie
- **THEN** it is redirected to the sign-in page

### Requirement: Panel requests are protected against forgery
The session cookie SHALL be inaccessible to scripts, sent only over HTTPS (except on a loopback address) and never on cross-site requests. Every state-changing panel request SHALL use `POST` and carry a per-session anti-forgery token; a request without a valid token MUST be rejected with `403` and change nothing. Panel pages SHALL forbid framing and inline scripts.

#### Scenario: Forged request
- **WHEN** a state-changing request arrives with a valid session cookie and no anti-forgery token
- **THEN** the response is `403` and nothing changes

### Requirement: Account management
The panel SHALL list accounts with address, state, last sign-in time and number of active sessions, and SHALL let the administrator create an account from an email address, disable and re-enable it, revoke its sessions and delete it. Deleting an account SHALL delete its rules and tokens. The configured administrator account MUST NOT be disabled or deleted from the panel.

#### Scenario: Create an account
- **WHEN** the administrator adds `ann@example.com`
- **THEN** the account appears as active with no rules, and that address can sign in

#### Scenario: Malformed address
- **WHEN** the administrator adds `not-an-address`
- **THEN** the request is rejected with an explanation and no account is created

#### Scenario: Delete an account
- **WHEN** the administrator deletes an account that has rules and valid tokens
- **THEN** its tokens stop working and its rules are gone

#### Scenario: Protecting the administrator
- **WHEN** the administrator tries to disable the configured administrator account
- **THEN** the request is rejected

### Requirement: Repository management
The panel SHALL list repositories with alias, remote URL, branch, status, time of last successful sync, last error and a credential hint (the last four characters of a token, or the fingerprint of an SSH key), and SHALL let the administrator add a repository, replace its credential, trigger a sync and remove it. For an SSH repository it SHALL show the public key with a way to copy it. Removal SHALL require typing the alias to confirm.

#### Scenario: Add a repository
- **WHEN** the administrator submits an alias, URL, branch and token
- **THEN** the repository appears with status `cloning` and later `ok` without a page-level restart

#### Scenario: Add an SSH repository with a generated key
- **WHEN** the administrator submits an alias, an SSH URL and the choice to generate a key
- **THEN** the repository page shows the public key to register with the Git host, the status `key_pending`, and an action to connect once the key is registered

#### Scenario: Replace a credential
- **WHEN** the administrator submits a new token or key for a repository in the `error` or `credential_invalid` state
- **THEN** the server retries with the new credential and the status reflects the outcome

#### Scenario: Removal confirmation
- **WHEN** the administrator requests removal and types a different alias
- **THEN** nothing is removed

### Requirement: Access rule management
The panel SHALL show, for any repository regardless of the administrator's own rules, its folder tree and, for a chosen folder, every account with its explicit rule (if any) and its effective level together with the folder the effective level is inherited from. The administrator SHALL be able to set a rule to `none`, `read` or `write` or to clear it so the folder inherits again.

#### Scenario: Grant on a nested folder
- **WHEN** the administrator sets `read` for an account on `notes/Shared/Team`
- **THEN** the account can read that folder at once, and the panel shows `read` as explicit there and as inherited on its subfolders

#### Scenario: Clearing a rule
- **WHEN** the administrator clears an account's `none` rule on `notes/Projects/Secret` while it has `write` on `notes/Projects`
- **THEN** the effective level on `notes/Projects/Secret` becomes `write`, shown as inherited from `notes/Projects`

#### Scenario: Folder removed from the repository
- **WHEN** a folder that carries a rule no longer exists after a sync
- **THEN** the rule is listed as orphaned and can be cleared

### Requirement: Rule overview per account
The panel SHALL show all rules of one account across repositories, and SHALL mark rules whose folder no longer exists so they can be cleared.

#### Scenario: Account overview
- **WHEN** the administrator opens an account that has rules in two repositories
- **THEN** every rule is listed with its repository, folder and level

### Requirement: The panel never exposes secrets or note content
The panel MUST NOT display repository tokens, SSH private keys, one-time codes, access tokens or the secret key, and MUST NOT display note content or file names; of a repository's content it SHALL show folder names only.

#### Scenario: Repository page source
- **WHEN** the repository page is rendered for a repository with a stored token
- **THEN** the response contains only the last four characters of the token

#### Scenario: Folder tree without content access
- **WHEN** the administrator, holding no rule on a repository, opens its folder tree in the panel
- **THEN** folder names are shown and no note title, file name or content is
