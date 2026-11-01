# MkDocsEditor accounts and authentication

This document is the contract between the backend and the clients (web, Android) for
accounts, registration, login, sessions and administration. It replaces section 3 of
[`sync-protocol.md`](sync-protocol.md) once implemented. The websocket protocol itself
does not change (protocol version 1 stays), only how a connection authenticates and a few
additive fields.

Status: **design**. The implementation lands in several pull requests; each section says
what is new. Where this document and `openapi-description.yaml` disagree on a REST
endpoint, the OpenAPI file wins once the endpoint is implemented.

Decisions:

- Registration by **invite links only**. No open sign-up, no email, no OIDC/SSO.
- The first admin is created with a one-time **setup link**, printed to the log on first
  start and on demand by `mkdocsrest setup-link`.
- Login with a **password** (Argon2id) with optional **TOTP** and recovery codes, and/or
  **passkeys** (WebAuthn, discoverable credentials) on the web and on Android.
- Password reset: an admin creates a single-use **reset link** and hands it over. A reset
  revokes all sessions of the account.
- Accounts live in an embedded **SQLite** database in the state directory.

## Contents

1. [Overview](#1-overview)
2. [Data model](#2-data-model)
3. [Credentials and tokens](#3-credentials-and-tokens)
4. [Request authentication](#4-request-authentication)
5. [CSRF and CORS](#5-csrf-and-cors)
6. [Websockets](#6-websockets)
7. [Links: setup, invite, reset](#7-links-setup-invite-reset)
8. [Flows](#8-flows)
9. [Endpoints](#9-endpoints)
10. [Errors](#10-errors)
11. [Rate limits](#11-rate-limits)
12. [Passkeys: RP ID, origins and Android](#12-passkeys-rp-id-origins-and-android)
13. [Configuration](#13-configuration)
14. [Migration from configured users](#14-migration-from-configured-users)
15. [Changes to protocol v1](#15-changes-to-protocol-v1)
16. [What clients must implement](#16-what-clients-must-implement)
17. [Threat model](#17-threat-model)

## 1. Overview

```
                  invite / setup / reset link (admin hands it over, or the log)
                                     |
                                     v
 web  --- cookie session ------>  backend  <------ bearer session --- Android
          (HttpOnly, SameSite=Lax)   |    (Authorization: Bearer mds_…)
                                     |
                         <stateDir>/auth.db (SQLite, WAL)
```

- Every account has a stable internal **id**. Usernames can change; everything that
  refers to a user (actor bindings, git attribution, audit log) uses the id.
- A successful login creates a **session**: one opaque random token per device. The web
  client receives it as an HttpOnly cookie and never sees it; Android receives it in the
  response body and sends it as a bearer token. There is one token per session, no
  separate refresh token: the session slides with use (idle timeout) up to an absolute
  lifetime.
- Optional **app tokens** (personal access tokens) are for scripts. They never reach the
  account or admin endpoints.
- Roles stay `admin`, `editor`, `reader` (see `sync-protocol.md` section 2). The role is
  read from the database on every request, so a role change applies immediately.

## 2. Data model

All timestamps are UTC, RFC 3339 in JSON. Ids are 26 lower-case base32 characters
(128 random bits), for example `k3x8m2q9w4e7r1t6y5u0i3o8pa`.

### Account

| Field | Notes |
|---|---|
| `id` | stable, never reused |
| `username` | 1–32 characters `[A-Za-z0-9._-]`, starting with a letter or digit, unique ignoring case. Never contains `:` (reserved for pseudo users like `:disk`). Former usernames stay reserved for the account (see below). |
| `displayName` | 1–64 characters, shown to others and used as git author name |
| `email` | optional, only used as git author email. The backend never sends email. |
| `color` | `#rrggbb`, default derived from the username |
| `role` | `admin`, `editor`, `reader` |
| `status` | `active`, `disabled` (cannot log in, sessions revoked), `deleted` (tombstone) |
| password | Argon2id hash, optional (passkey-only accounts have none) |
| TOTP | encrypted secret, last used time step, optional |
| recovery codes | 10 single-use codes, keyed hashes |
| passkeys | WebAuthn credentials, any number |
| `createdAt`, `lastLoginAt` | |

**Username changes.** When a username changes, the old name stays reserved for the account
(an alias). Nobody else can take it, and git commits made under the old name
(`<oldname>@mkdocseditor.invalid`) still resolve to the account.

**Deletion is a tombstone.** A deleted account keeps its id, username, display name and
colour, so authorship, history and the audit log still show who wrote what. Everything
else is removed: password, TOTP, recovery codes, passkeys, sessions, app tokens. The
username stays reserved. Deleted accounts are listed by the admin API with
`status: "deleted"` and cannot be restored.

### Session

| Field | Notes |
|---|---|
| `id` | public id, used to list and revoke sessions |
| token | 256 random bits, stored as SHA-256 hash only |
| `type` | `cookie` (web) or `bearer` (Android and other native clients) |
| `deviceName` | given by the client at login (for example "Pixel 8"), else derived from the user agent |
| `userAgent`, `ip` | at creation, `ip` updated with `lastUsedAt` |
| `createdAt`, `lastUsedAt` | `lastUsedAt` is written at most every 5 minutes |
| `expiresAt` | absolute expiry: `createdAt` + `sessionMaxLifetime` (default 90 days) |
| `idleExpiresAt` | `lastUsedAt` + `sessionIdleTimeout` (default 30 days) |
| `reauthenticatedAt` | last login or step-up, see [Step-up](#step-up-re-authentication) |

### App token

`id`, `name`, `scope` (`read` or `write`), `createdAt`, `lastUsedAt`, `expiresAt`
(optional). Token stored as SHA-256 hash. The effective rights are the intersection of
the scope and the account's role: a `write` token of a reader can only read.

### Link

Setup, invite and reset links share one table: `id`, `kind` (`setup`, `invite`,
`reset`), token hash, `role` (setup: always `admin`; invite: chosen), `accountId`
(reset only), `note` (invite only, free text up to 200 characters, visible to admins
and to the person redeeming it), `resetSecondFactors` (reset only), `createdBy`,
`createdAt`, `expiresAt`, `usedAt`, `usedBy`, `revokedAt`. Links are single use.

### Device link

Pairs a new device (the Android app) with an account that is signed in elsewhere (the web
client), see [Device pairing](#device-pairing-connect-the-android-app). Fields: `id`,
`accountId`, token hash, `deviceName` (optional hint from the creator, up to 64
characters), `createdBySession`, `createdAt`, `expiresAt` (creation + 5 minutes),
`redeemedAt`, `redeemedSession` (the session it created), `redeemedDeviceName`,
`revokedAt`. Single use. Rows are deleted 24 hours after they expire.

### Actor binding

`actorId` → `subject`, where `subject` is an account id, a pseudo user (`:disk`,
`:server`) or `anonymous` (only with `server.auth.disabled`). This replaces
`actors.json`, see [Migration](#14-migration-from-configured-users).

### Audit event

`id` (increasing integer), `time`, `type`, `actorId` (account that acted, may be empty),
`targetId` (affected account, may be empty), `ip`, `userAgent`, `details` (object).

Types: `login.succeeded`, `login.failed`, `login.second-factor-failed`, `logout`,
`session.revoked`, `reauth.succeeded`, `reauth.failed`, `password.changed`,
`password.removed`, `totp.enabled`, `totp.disabled`, `recovery-codes.generated`,
`recovery-code.used`, `passkey.added`, `passkey.renamed`, `passkey.removed`,
`app-token.created`, `app-token.revoked`, `device-link.created`,
`device-link.revoked`, `device-link.redeemed`, `setup.link-created`, `setup.completed`,
`invite.created`, `invite.revoked`, `invite.redeemed`, `reset.created`,
`reset.revoked`, `reset.redeemed`, `account.created`, `account.imported`,
`account.updated`, `account.role-changed`, `account.disabled`, `account.enabled`,
`account.deleted`, `settings.changed`, `rate-limit.triggered`.

`login.failed` events for unknown usernames are recorded with the attempted username in
`details` (truncated to 64 characters) and no `targetId`. Events are kept for
`server.auth.auditRetention` (default 365 days).

### Instance settings

| Key | Type | Default | Meaning |
|---|---|---|---|
| `require2FA` | bool | `false` | Every account must have a second factor: TOTP or at least one passkey. Accounts without one can log in with their password but only reach the endpoints needed to set one up. |

### Storage

`<stateDir>/auth.db`, SQLite through the pure Go driver `modernc.org/sqlite` (no extra
C dependency), journal mode WAL, `synchronous=NORMAL`, foreign keys on, busy timeout 5 s.
The schema version is `PRAGMA user_version`; migrations run at startup inside a
transaction and only forward. File mode 0600.

**Backup:** the database is one file. With the server running, use
`sqlite3 auth.db ".backup auth-backup.db"` or copy it while the server is stopped (a ZFS
or volume snapshot of the state directory is also consistent, thanks to WAL). Back up
`auth.key` too, otherwise TOTP secrets and recovery codes in a restored database are
useless (accounts then log in with password or passkey and re-enroll TOTP after an
admin reset with `resetSecondFactors`).

## 3. Credentials and tokens

### Passwords

- Argon2id, OWASP parameters: memory 19 MiB (`m=19456`), iterations 2, parallelism 1,
  16-byte random salt, 32-byte key, stored in PHC format
  (`$argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>`). At most 4 hashes are computed at the
  same time, further requests wait.
- Rules (NIST SP 800-63B): Unicode NFKC normalized; at least **10** and at most **256**
  characters (code points); no composition rules; no periodic expiry; rejected when it is
  in the built-in blocklist of common and breached passwords, consists of one repeated
  character or a simple sequence (`1234567890`, `abcdefghij`), or contains the username
  or the words `mkdocs` / `mkdocseditor` (ignoring case). All characters are allowed,
  including spaces. `GET /auth/status/` returns the limits for form validation.
- Hashes imported from the configuration (bcrypt) verify as bcrypt and are replaced by an
  Argon2id hash at the next successful login. Hashes with older Argon2id parameters are
  upgraded the same way.
- Unknown usernames are verified against a dummy hash, so the response time does not
  reveal whether an account exists.

### Token formats

| Token | Format | Where |
|---|---|---|
| session | `mds_` + 43 base64url characters (32 random bytes) | cookie value or `Authorization: Bearer` |
| app token | `mdp_` + 43 base64url characters | `Authorization: Bearer` |
| link token | `mdl_` + 43 base64url characters | link fragment, request bodies |
| login challenge | `mdc_` + 43 base64url characters | between password and second factor |
| device link token | `mdd_` + 43 base64url characters | connect link payload, redeem request body |
| websocket ticket | 43 base64url characters (unchanged) | first websocket message |

Tokens are compared by looking up their SHA-256 hash, so no timing difference depends on
the secret. The prefixes make leaked tokens easy to find with secret scanners. Clients
treat tokens as opaque strings.

### TOTP

RFC 6238: HMAC-SHA1, 6 digits, 30-second steps, secret of 20 random bytes (base32 in
the enrollment response). The server accepts the current step and one step on each side
(±30 s). A step that was used once is never accepted again for that account (replay
protection: the last used step is stored). Secrets are encrypted at rest with AES-256-GCM
using the server key (see [Configuration](#13-configuration)).

### Recovery codes

10 codes per account, each `xxxx-xxxx-xxxx` from the alphabet `0-9a-z` without
`0 o 1 l` (about 60 bits). Input ignores case, spaces and dashes. Stored as
HMAC-SHA256 with the server key; each code works once. Regenerating replaces all codes.
Codes are shown exactly once, in the response that creates them.

## 4. Request authentication

Each request is authenticated by the first matching method:

1. `Authorization: Bearer mds_…`: a bearer session.
2. `Authorization: Bearer mdp_…`: an app token.
3. `Authorization: Basic …` with the username and an **app token** as the password, for
   tools that only speak Basic auth. This always works, since no password is sent.
   Username and **account password** only when `server.auth.legacyBasicAuth: true`
   (default `false`), see [Migration](#14-migration-from-configured-users), and only for
   accounts **without** a second factor.
4. The session cookie (`__Host-mkdocseditor_session` over HTTPS,
   `mkdocseditor_session` over plain HTTP).

If an `Authorization` header is present and invalid, the request fails with 401; the
cookie is not tried. An expired or revoked session gives 401 `unauthenticated`, and over
the cookie the response also clears the cookie.

Without any method, endpoints that need authentication return 401 `unauthenticated`. No
`WWW-Authenticate: Basic` header is sent any more unless legacy Basic auth is enabled
and the request has no `X-Requested-With` header.

**Authorization by route group:**

| Routes | Who |
|---|---|
| `GET /alive/`, `GET /auth/status/`, the pre-login `/auth/…` endpoints, `GET /.well-known/assetlinks.json` | everyone, rate limited |
| content reads (`GET` on `/section/`, `/document/`, `/resource/`, `/mkdocs/`, `/actors/`, websockets) | any role; app tokens with `read` or `write` |
| content changes (`POST`, `PUT`, `DELETE` on content routes, history restore) | `editor`, `admin`; app tokens with `write` |
| `/auth/whoami/`, `/auth/logout/`, `/auth/ws-ticket/`, `/auth/reauth/…` | sessions (any role); `whoami` and `ws-ticket` also app tokens |
| `/account/…` | sessions only |
| `/admin/…` | sessions of admins only |

A session of an account that must still set up a second factor (`require2FA`) is
**restricted**: it reaches only `GET /auth/whoami/`, `POST /auth/logout/`,
`/auth/reauth/…`, `GET /account/`, the TOTP and passkey enrollment endpoints, and
`GET /auth/status/`. Everything else returns 403 `second-factor-setup-required`.

With `server.auth.disabled: true` every request is the anonymous admin (development
only; the server warns at startup), and the account endpoints return 404.

## 5. CSRF and CORS

Cookies are sent automatically by browsers, so cookie-authenticated requests are
protected against cross-site request forgery:

- **Custom header.** Every request with a method other than `GET`, `HEAD` or `OPTIONS`
  that has no `Authorization` header must carry `X-Requested-With` with any non-empty
  value (the web client sends `XMLHttpRequest`). Otherwise: 403 `csrf-check-failed`.
  This includes the login and link endpoints (login CSRF). A cross-origin page cannot
  add this header without a CORS preflight, and preflights succeed only for configured
  origins.
- **Origin check.** For the same requests, an `Origin` header must be the backend's own
  origin or listed in `server.cors.allowedOrigins`; a request that carries the session
  cookie but no `Origin` header is refused. Otherwise: 403 `origin-not-allowed`. Native
  clients send no `Origin` and no cookie, so they pass (they still send
  `X-Requested-With` on unauthenticated requests like login).
- **SameSite=Lax** on the session cookie as defence in depth.
- Requests with an `Authorization` header are not checked: browsers never add it on
  their own.

**CORS.** With `server.cors.allowedOrigins` set, those origins get
`Access-Control-Allow-Credentials: true`, so `fetch(…, {credentials: "include"})` works.
Allowed methods (default of `server.cors.allowedMethods`): `HEAD`, `GET`, `POST`, `PUT`,
`PATCH`, `DELETE`. Allowed request headers: `Authorization`, `Content-Type`, `Accept`,
`X-Requested-With`.
Exposed response headers: `Content-Disposition`, `Retry-After`. Without configured
origins every origin may read responses but no credentials are allowed, so a web client
on another origin cannot log in. List the web client's origin, also for development
(`http://localhost:5173`).

**Same-site requirement for cookies.** The web client and the backend must be
*same-site* (same registrable domain, scheme ignored for `localhost`), for example
`edit.wiki.markusressel.de` and `backend.wiki.markusressel.de`, or `localhost:5173`
and `localhost:7413`. Then the Lax cookie is sent on `fetch` and on the websocket
upgrade. The cookie is host-only (no `Domain` attribute), so it goes only to the backend
host. A web client on an unrelated domain must use bearer tokens instead (not supported
by the official web client).

## 6. Websockets

`GET /document/{id}/ws/` and `GET /events/ws/` authenticate with one of:

1. the **session cookie** on the upgrade request (browsers; the `Origin` header must be
   the backend's own origin or a configured origin, otherwise the cookie is ignored),
2. `Authorization: Bearer mds_…` or `mdp_…` on the upgrade request (Android/OkHttp,
   scripts),
3. a **ticket** in the `hello` message, as before: `POST /auth/ws-ticket/` returns
   `{"ticket": "…", "expiresAt": "…"}`, valid for 60 s, single use. Tickets keep working
   for every client and are the fallback when the upgrade cannot carry a cookie.
4. `Authorization: Basic …` with an app token, or with the account password when legacy
   Basic auth is enabled.

A ticket remembers the session it was issued for and is refused when that session ended
in the meantime. When a session is revoked, an account is disabled or deleted, its
password is reset, or its role changes, the server closes that account's (or that
session's) websockets with close code `4001` (revoked, disabled, deleted, reset) or
`4003` (role changed). The client then reconnects, which applies the new role, or
shows the login.

Tickets are never accepted in the URL.

## 7. Links: setup, invite, reset

A link carries a link token in the URL **fragment**, so it never reaches a web server's
access log:

```
{clientUrl}/setup#mdl_…
{clientUrl}/invite#mdl_…
{clientUrl}/reset#mdl_…
```

`clientUrl` is `server.auth.clientUrl` (the web client, for example
`https://edit.wiki.markusressel.de`). The API returns both the `token` and the full
`url` (`url` is `null` when `clientUrl` is not configured; clients can then build
their own link or show the token). Clients read the token from the fragment and send it
in request bodies only; the backend never accepts link tokens in URLs.

| Kind | Created by | Default expiry | Max expiry | Effect |
|---|---|---|---|---|
| `setup` | the server on start while no account exists, `mkdocsrest setup-link` | 24 h | 24 h | creates the first account, role `admin` |
| `invite` | admins | 7 days | 30 days | creates an account with the invite's role |
| `reset` | admins, per account | 1 h | 24 h | sets a new password and/or adds a passkey; revokes all sessions; optionally removes TOTP, recovery codes and passkeys first |

- **Setup.** On every start without accounts (and without users to import), the server
  revokes older setup links, creates a new one and prints it on stdout in two lines that
  tests can parse:

  ```
  setup link: https://edit.wiki.markusressel.de/setup#mdl_…
  setup token: mdl_…
  ```

  The lines go to stdout and are flushed right away. The `setup link:` line is only
  printed when `clientUrl` is set. `mkdocsrest setup-link`
  (same `--config`) prints the same two lines for a fresh token while the server keeps
  running (both use the same database; the newest setup link revokes the older ones). It
  refuses with exit code 1 when an account exists. Setup links stop working as soon as
  any account exists.
- **Invite.** Single use. Admins can list and revoke invites. The redeemer chooses the
  username and display name.
- **Reset.** Only for `active` accounts. Creating a new reset link for an account revokes
  its older reset links. Redeeming signs the user in with a new session, after revoking
  all other sessions and app tokens of the account. With `resetSecondFactors: true`
  (for a lost phone) TOTP, recovery codes and all passkeys are removed first.

## 8. Flows

All request bodies are JSON (`Content-Type: application/json`). Browser requests use
`credentials: "include"` and the `X-Requested-With` header on non-`GET` requests.

### Session delivery

Every endpoint that creates a session (login, second factor, passkey login, link
redeem) takes:

| Field | Values |
|---|---|
| `sessionType` | `"cookie"` (default, web) or `"bearer"` (Android) |
| `deviceName` | optional, up to 64 characters |

and answers with a **session response**:

```json
{
  "status": "ok",
  "user": { "id": "k3x8…", "username": "markus", "displayName": "Markus", "role": "admin", "color": "#e8590c", "secondFactorSetupRequired": false, "passkeyCount": 0 },
  "session": { "id": "p1q2…", "type": "bearer", "deviceName": "Pixel 8", "createdAt": "…", "expiresAt": "…", "idleExpiresAt": "…" },
  "token": "mds_…"
}
```

`user.passkeyCount` is the number of passkeys of the account, for the passkey step
after sign-in (see [Step-up](#step-up-re-authentication)). `token` is only present for
`bearer` sessions. For `cookie` sessions the response sets
the cookie:

```
Set-Cookie: __Host-mkdocseditor_session=mds_…; Path=/; Max-Age=7776000; HttpOnly; Secure; SameSite=Lax
```

(over plain HTTP: name `mkdocseditor_session`, no `Secure`, see `cookieSecure`).

### First start (setup)

1. Operator opens the setup link from the log (or runs `mkdocsrest setup-link`).
2. Client: `POST /auth/link/inspect/` `{"token": "mdl_…"}` →
   `{"kind": "setup", "role": "admin", "expiresAt": "…"}`.
3. Client asks for username, display name and a password **or** a passkey:
   - password: `POST /auth/link/redeem/` →  session response
   - passkey: `POST /auth/link/passkey/begin/` then `…/finish/`, see
     [Passkey registration](#passkey-registration) → session response
4. The setup link is used up; `GET /auth/status/` now returns `setupRequired: false`.

### Invite

1. Admin: `POST /admin/invites/` `{"role": "editor", "expiresInHours": 168, "note": "for Anna"}`
   → `{"invite": {…}, "token": "mdl_…", "url": "https://…/invite#mdl_…"}`. The admin
   shares the URL or shows it as a QR code (generated by the client).
2. Invitee opens the link: inspect, then redeem with password or passkey, exactly like
   setup. The response is a session; the account exists from then on.
3. With `require2FA`, the new session is restricted until a second factor is set up
   (`user.secondFactorSetupRequired: true`). An account created with a passkey already
   has one.

### Password login

```
POST /auth/login/
{"username": "markus", "password": "…", "sessionType": "cookie", "deviceName": "Firefox on Linux"}
```

- Wrong username or password, disabled account with a wrong password: 401
  `invalid-credentials` (same message, same timing).
- Correct password, no second factor: session response.
- Correct password, the account has TOTP or passkeys: no session yet, but

  ```json
  {"status": "second-factor-required", "challenge": "mdc_…", "methods": ["totp", "recovery-code", "passkey"], "expiresAt": "…"}
  ```

  `methods` lists what the account has (`passkey` only when passkeys are enabled and the
  account has one). The challenge is valid for 5 minutes and 5 attempts.
- Correct password but the account is disabled: 403 `account-disabled`.

Second factor with TOTP or a recovery code:

```
POST /auth/login/second-factor/
{"challenge": "mdc_…", "code": "123456", "sessionType": "cookie", "deviceName": "…"}
{"challenge": "mdc_…", "recoveryCode": "abcd-efgh-jkmn", "sessionType": "cookie"}
```

→ session response, or 401 `invalid-second-factor` (attempts left in
`details.attemptsLeft`), or 401 `challenge-expired` (expired, used, or out of attempts:
start over with the password). A used recovery code is spent; `GET /account/` shows how
many are left.

Second factor with a passkey: `POST /auth/passkey/login/begin/` with
`{"challenge": "mdc_…"}`, then `…/finish/` (the options then list the account's
credentials in `allowCredentials`).

### Passkey login (discoverable, no username)

```
POST /auth/passkey/login/begin/
{}
→ {"ceremonyId": "…", "publicKey": { "challenge": "…", "rpId": "markusressel.de", "timeout": 300000, "userVerification": "required" }}
```

The client passes `publicKey` to `navigator.credentials.get({publicKey, mediation})`
(after decoding the base64url fields; `@simplewebauthn/browser`
`startAuthentication({optionsJSON: publicKey})` takes it as is) or to Android Credential
Manager (`GetPublicKeyCredentialOption(requestJson = publicKey as JSON)`). Conditional
UI (`mediation: "conditional"`) works because `allowCredentials` is empty.

```
POST /auth/passkey/login/finish/
{"ceremonyId": "…", "credential": { "id": "…", "rawId": "…", "type": "public-key", "response": { "clientDataJSON": "…", "authenticatorData": "…", "signature": "…", "userHandle": "…" }, "clientExtensionResults": {} }, "sessionType": "bearer", "deviceName": "Pixel 8"}
→ session response
```

`credential` is the standard JSON serialization of a `PublicKeyCredential`
(`PublicKeyCredential.toJSON()` in browsers, `AuthenticationResponseJSON` of
simplewebauthn, the `authenticationResponseJson` of Credential Manager), binary fields
base64url without padding. A passkey login needs user verification and counts as two
factors: no TOTP step follows. Errors: 401 `passkey-verification-failed` (unknown
credential, bad signature, wrong origin or RP ID), 401 `challenge-expired` (ceremony
older than 5 minutes or used).

### Passkey registration

Same two steps for the three places a passkey is created:

| Place | begin | finish |
|---|---|---|
| setup or invite (new account) | `POST /auth/link/passkey/begin/` `{"token", "username", "displayName"}` | `POST /auth/link/passkey/finish/` `{"ceremonyId", "credential", "passkeyName", "sessionType", "deviceName"}` → session response |
| reset (existing account) | `POST /auth/link/passkey/begin/` `{"token"}` | same finish → session response |
| signed in | `POST /account/passkeys/begin/` `{}` (step-up) | `POST /account/passkeys/finish/` `{"ceremonyId", "credential", "name"}` → passkey object |

`begin` returns `{"ceremonyId": "…", "publicKey": {…}}` where `publicKey` is a
`PublicKeyCredentialCreationOptionsJSON`: `rp {id, name}`, `user {id, name,
displayName}` (`user.id` is a random per-account WebAuthn handle, not the account id),
`challenge`, `pubKeyCredParams` (ES256, EdDSA, RS256), `timeout` 300000,
`excludeCredentials` (the account's existing passkeys), `authenticatorSelection
{residentKey: "required", requireResidentKey: true, userVerification: "required"}`,
`attestation: "none"`. Pass it to `navigator.credentials.create`,
`startRegistration({optionsJSON: publicKey})` or Credential Manager
(`CreatePublicKeyCredentialRequest(requestJson)`). `credential` in finish is the
standard JSON of the new credential (`RegistrationResponseJSON`, including
`response.transports` when available). `passkeyName`/`name` is optional (up to 64
characters, default: authenticator name from the AAGUID if known, else "Passkey").

**Password managers.** Passkeys created by third-party providers (Bitwarden, 1Password,
Google Password Manager, iCloud Keychain) work on the web and on Android: attestation is
`none`, any AAGUID and authenticator is accepted, and an account can have any number of
passkeys. Synced passkeys report `backedUp: true`. TOTP is plain RFC 6238 (SHA1, 6
digits, 30 s) with a standard `otpauth://` URI, so every password manager can store it.
The API field names stay plain (`username`, `password`, `currentPassword`,
`newPassword`); clients set the `autocomplete` attributes (`username`,
`current-password`, `new-password`, `one-time-code`, `webauthn`) so password managers
fill and save them.

For link begin, the username is checked (409 `username-taken`, 400 `invalid-username`)
but not reserved; finish checks again.

### Step-up (re-authentication)

Sensitive actions need a recent authentication of the session: within the last
**10 minutes**. A fresh login counts. Otherwise they answer 403 `reauth-required` and the
client asks the user to confirm and calls one of:

```
POST /auth/reauth/
{"password": "…"}                      account without TOTP
{"password": "…", "code": "123456"}    account with TOTP (password and code)
{"code": "123456"}                     account without password (passkey-only with TOTP; rare)
→ {"reauthenticatedUntil": "…"}
```

or a passkey: `POST /auth/reauth/passkey/begin/` `{}` →
`{"ceremonyId", "publicKey"}` (with the account's `allowCredentials`), then
`POST /auth/reauth/passkey/finish/` `{"ceremonyId", "credential"}` →
`{"reauthenticatedUntil": "…"}`. A passkey always suffices. Failures count against the
account's login backoff and give 401 `invalid-credentials`.

Sensitive actions: change or remove the password (unless `currentPassword` is sent),
change one's own username, TOTP enroll and disable, regenerate recovery codes, add or
delete passkeys, create app tokens and device links, and the admin actions: change a role, disable,
delete, create reset links, change settings.

**Passkey right after sign-in.** Clients suggest creating a passkey right after the
first sign-in on a device (setup, invite, password login, device pairing). So that this
needs no extra prompt, `POST /account/passkeys/begin/` also accepts a session that was
created less than 10 minutes ago, by any complete authentication: a login (with its
second factor, if the account has one), a link redeem, or a device link redeem. Clients
decide whether to show the step from `user.passkeyCount` in the session response (or
`passkeyCount` in `GET /account/`): offer it when it is `0`, or when this device has no
passkey of its own yet and the user wants one.

Step-up does not issue a new token. Tokens are only ever created after a complete
authentication (no pre-login session is upgraded in place), so session fixation is not
possible; see the threat model.

### Reset

1. Admin: `POST /admin/users/{id}/reset-link/` `{"expiresInMinutes": 60, "resetSecondFactors": false}`
   (step-up) → `{"link": {…}, "token": "mdl_…", "url": "…/reset#mdl_…"}`.
2. User opens the link: `POST /auth/link/inspect/` →
   `{"kind": "reset", "username": "anna", "displayName": "Anna", "resetSecondFactors": false, "expiresAt": "…"}`.
3. User sets a new password (`POST /auth/link/redeem/` `{"token", "password",
   "sessionType", "deviceName"}`) or registers a passkey (link passkey begin/finish;
   the password, if any, stays as it is).
4. All sessions and app tokens of the account are revoked, then the new session is
   returned. If the account still has TOTP, the reset sign-in does **not** ask for it
   (the admin vouched for the user); with `require2FA` and no second factor left the
   session is restricted.

### Device pairing (connect the Android app)

A user who is signed in on the web connects the Android app without typing the server
address, username or password:

1. Web (signed-in session, step-up): `POST /account/device-links/` `{"deviceName": "Pixel 8"}`
   (`deviceName` optional) →

   ```json
   {"deviceLink": {device link object}, "token": "mdd_…", "expiresAt": "…", "connectUrl": "https://edit.wiki.markusressel.de/connect#eyJ2IjoxLC…"}
   ```

   The web client shows `connectUrl` as a QR code and as a link to tap on the phone.
2. Android scans the QR code or opens the link (App Link on `/connect`, or pasted) and
   decodes the payload (below).
3. Android: `GET {backendUrl}/server-info/` to show the instance name and check the
   protocol version, then `POST {backendUrl}/auth/device-link/redeem/`
   `{"token": "mdd_…", "username": "markus", "deviceName": "Pixel 8", "sessionType": "bearer"}`
   → session response (with `token`).
4. Web: `GET /account/device-links/{id}/` shows `status: "redeemed"` and
   `redeemedDeviceName`; the new session also appears in `GET /account/sessions/`.

**Connect link.** `connectUrl` is `{clientUrl}/connect#<payload>`, `null` when
`clientUrl` is not configured. The fragment is the whole payload: the UTF-8 JSON object
below, encoded as base64url without padding. The payload never reaches a server log.

```json
{"v": 1, "backendUrl": "https://backend.wiki.markusressel.de", "username": "markus", "token": "mdd_…", "expiresAt": "2026-11-01T12:05:00Z"}
```

| Field | Notes |
|---|---|
| `v` | payload version, `1`. Clients reject other versions with "update the app". |
| `backendUrl` | the API base URL: `server.backendUrl`, else derived from the request that created the link (scheme, honouring `X-Forwarded-Proto` from trusted proxies, and `Host`) |
| `username` | the account's current username, for display and the redeem check |
| `token` | the device link token |
| `expiresAt` | so the client can say "expired" without a request |

Clients ignore unknown fields. The payload is about 200 characters, small enough for a
QR code with error correction level M.

**Rules.**

- Creating needs a session (not an app token) with a recent authentication
  ([Step-up](#step-up-re-authentication)); a fresh login counts, for cookie and bearer
  sessions alike. Restricted sessions (`require2FA`) cannot create device links.
- The token is valid for **5 minutes** and **once**. An account has at most 5 pending
  device links; creating a sixth revokes the oldest.
- Redeeming creates a normal session of the account, like a login. The second factor
  counts as satisfied, because the creator passed step-up. The new session is **not**
  recently authenticated, with one exception: like every new session it may register a
  passkey during its first 10 minutes (see [Step-up](#step-up-re-authentication)). All
  other sensitive actions on the new device need their own step-up.
- `username` in the redeem request is optional; when sent, it must match the account
  (ignoring case), otherwise the request fails like an unknown token.
- `sessionType` defaults to `"bearer"`; `"cookie"` is also accepted.
- Unknown, used, expired and revoked tokens, and a wrong `username`, all give 400
  `invalid-link`. A disabled account gives 403 `account-disabled`.
- The redeem endpoint is unauthenticated: it needs `X-Requested-With` like the other
  unauthenticated `POST` requests (section 5) and counts against the per-IP limit.
- Pending device links are revoked when the account's password changes, on a reset, and
  when the account is disabled or deleted.
- Audit events: `device-link.created`, `device-link.revoked`, `device-link.redeemed`
  (with the device name and the new session id in `details`).

### Logout and sessions

- `POST /auth/logout/` ends the current session (cookie cleared) → 204.
- `GET /account/sessions/`, `DELETE /account/sessions/{id}/`,
  `POST /account/sessions/revoke-others/`.
- A session ends at `idleExpiresAt` or `expiresAt`, whichever comes first. There is no
  refresh call: every authenticated request extends the idle expiry. Clients handle 401
  `unauthenticated` by showing the login.
- Changing the password revokes all **other** sessions and all app tokens of the
  account; the current session stays.

## 9. Endpoints

Trailing slashes as in the rest of the API (requests without one are redirected
internally). "Session response", "user object" and so on refer to section 8. All error
responses use the format of [Errors](#10-errors).

### Objects

**User object** (also in `whoami`, admin lists add more fields):

```json
{"id": "k3x8…", "username": "markus", "displayName": "Markus", "role": "admin", "color": "#e8590c"}
```

**Session object:**

```json
{"id": "p1q2…", "type": "cookie", "deviceName": "Firefox on Linux", "userAgent": "Mozilla/5.0 …", "ip": "192.168.2.185",
 "createdAt": "…", "lastUsedAt": "…", "expiresAt": "…", "idleExpiresAt": "…", "current": true}
```

**Passkey object:**

```json
{"id": "f7g8…", "name": "Pixel 8", "createdAt": "…", "lastUsedAt": "…", "backedUp": true, "transports": ["internal", "hybrid"], "provider": "Bitwarden"}
```

`lastUsedAt` is `null` until the first use. `provider` is the name of a known passkey
provider (from the AAGUID) and absent for unknown authenticators.

**App token object:** `{"id", "name", "scope", "createdAt", "lastUsedAt", "expiresAt"}`.

**Device link object:**

```json
{"id": "…", "deviceName": "Pixel 8", "createdAt": "…", "expiresAt": "…", "status": "pending",
 "redeemedAt": null, "redeemedDeviceName": null, "sessionId": null}
```

`status`: `pending`, `redeemed`, `expired`, `revoked`. `deviceName` is the creator's hint
(`null` when none was given); `redeemedDeviceName` and `sessionId` are set once redeemed.

**Server info:**

```json
{"name": "wiki.markusressel.de", "version": "1.4.0", "protocolVersion": 1,
 "backendUrl": "https://backend.wiki.markusressel.de", "wikiUrl": "https://wiki.markusressel.de",
 "webClientUrl": "https://edit.wiki.markusressel.de", "setupRequired": false, "authDisabled": false,
 "passkeys": {"enabled": true, "rpId": "markusressel.de"},
 "loginMethods": ["password", "passkey", "device-link"], "require2FA": false}
```

- `name`: `server.instanceName`, else the host of `wikiUrl`, else the host of
  `webClientUrl`, else the host of the request.
- `version`: the backend build version (`dev` for local builds); `protocolVersion`: the
  sync protocol version (`sync-protocol.md`).
- `backendUrl`: as in the connect link payload. `wikiUrl`: `server.wikiUrl`, the
  published site. `webClientUrl`: `server.auth.clientUrl`. Each is `null` when unknown.
- `loginMethods`: `password` (always, unless `authDisabled`), `passkey` (when passkeys
  are enabled), `device-link` (always, unless `authDisabled`). Empty with `authDisabled`.
- Nothing secret and nothing about accounts (no user counts, no usernames). `/auth/status/`
  stays as it is; `/server-info/` is the superset for onboarding screens.

**Link object:**

```json
{"id": "…", "kind": "invite", "role": "editor", "note": "for Anna", "accountId": null, "resetSecondFactors": false,
 "createdBy": {"id": "…", "username": "markus"}, "createdAt": "…", "expiresAt": "…",
 "usedAt": null, "usedBy": null, "revokedAt": null, "status": "pending"}
```

`status`: `pending`, `used`, `expired`, `revoked`. Tokens are never returned again after
creation.

### Public

| Method | Path | Request | Response |
|---|---|---|---|
| GET | `/auth/status/` | | `{"setupRequired": false, "authDisabled": false, "passkeys": {"enabled": true, "rpId": "markusressel.de"}, "passwordPolicy": {"minLength": 10, "maxLength": 256}, "legacyBasicAuth": false}` |
| POST | `/auth/login/` | `{username, password, sessionType?, deviceName?}` | session response, or `second-factor-required` |
| POST | `/auth/login/second-factor/` | `{challenge, code}` or `{challenge, recoveryCode}`, plus `sessionType?, deviceName?` | session response |
| POST | `/auth/passkey/login/begin/` | `{}` or `{challenge}` | `{ceremonyId, publicKey}` |
| POST | `/auth/passkey/login/finish/` | `{ceremonyId, credential, sessionType?, deviceName?}` | session response |
| POST | `/auth/link/inspect/` | `{token}` | setup/invite: `{kind, role, note, expiresAt}`; reset: `{kind, username, displayName, resetSecondFactors, expiresAt}` |
| POST | `/auth/link/redeem/` | setup/invite: `{token, username, displayName?, password, sessionType?, deviceName?}`; reset: `{token, password, sessionType?, deviceName?}` | session response |
| POST | `/auth/link/passkey/begin/` | setup/invite: `{token, username, displayName?}`; reset: `{token}` | `{ceremonyId, publicKey}` |
| POST | `/auth/link/passkey/finish/` | `{ceremonyId, credential, passkeyName?, sessionType?, deviceName?}` | session response |
| GET | `/.well-known/assetlinks.json` | | Digital Asset Links, only when configured, see section 12 |
| GET | `/server-info/` | | server info object |
| POST | `/auth/device-link/redeem/` | `{token, username?, deviceName?, sessionType?}` (`sessionType` default `bearer`) | session response |

`displayName` defaults to the username. Invalid, used, expired or revoked link tokens all
give 400 `invalid-link`.

### Authenticated

| Method | Path | Request | Response |
|---|---|---|---|
| GET | `/auth/whoami/` | | user object plus `"authMethod": "session" \| "app-token" \| "basic" \| "anonymous"`, `"secondFactorSetupRequired": bool`, `"sessionId"` (sessions only) |
| POST | `/auth/logout/` | | 204 |
| POST | `/auth/ws-ticket/` | | `{ticket, expiresAt}` (unchanged) |
| POST | `/auth/reauth/` | `{password?, code?}` | `{reauthenticatedUntil}` |
| POST | `/auth/reauth/passkey/begin/` | `{}` | `{ceremonyId, publicKey}` |
| POST | `/auth/reauth/passkey/finish/` | `{ceremonyId, credential}` | `{reauthenticatedUntil}` |

### Account (sessions only)

| Method | Path | Step-up | Request | Response |
|---|---|---|---|---|
| GET | `/account/` | | | `{user…, "email", "createdAt", "hasPassword", "totpEnabled", "recoveryCodesLeft", "passkeyCount", "secondFactorSetupRequired", "require2FA", "reauthenticatedUntil"}` |
| PATCH | `/account/` | only for `username` | `{displayName?, color?, email?, username?}` (`email: ""` removes it) | as `GET /account/` |
| PUT | `/account/password/` | unless `currentPassword` is sent | `{currentPassword?, newPassword}` | 204 |
| DELETE | `/account/password/` | yes | | 204; 409 `last-login-method` without a passkey |
| POST | `/account/totp/` | yes | `{}` | `{"secret": "JBSW…", "otpauthUri": "otpauth://totp/MkDocsEditor:markus?secret=…&issuer=MkDocsEditor&algorithm=SHA1&digits=6&period=30", "expiresAt": "…"}`; 409 `totp-already-enabled` |
| POST | `/account/totp/confirm/` | | `{code}` | `{"recoveryCodes": ["…", … 10]}`; 400 `invalid-second-factor` |
| DELETE | `/account/totp/` | yes | | 204; 409 `second-factor-required` when `require2FA` and no passkey |
| POST | `/account/recovery-codes/` | yes | `{}` | `{"recoveryCodes": [10]}`; 409 `totp-not-enabled` |
| GET | `/account/passkeys/` | | | `[passkey object]` |
| POST | `/account/passkeys/begin/` | yes | `{}` | `{ceremonyId, publicKey}` |
| POST | `/account/passkeys/finish/` | | `{ceremonyId, credential, name?}` | passkey object |
| PATCH | `/account/passkeys/{id}/` | | `{name}` | passkey object |
| DELETE | `/account/passkeys/{id}/` | yes | | 204; 409 `last-login-method` (no password, last passkey), 409 `second-factor-required` (`require2FA`, no TOTP, last passkey) |
| GET | `/account/sessions/` | | | `[session object]`, current first |
| DELETE | `/account/sessions/{id}/` | | | 204 (the current session too: that is a logout) |
| POST | `/account/sessions/revoke-others/` | | | `{"revoked": 3}` |
| GET | `/account/tokens/` | | | `[app token object]` |
| POST | `/account/tokens/` | yes | `{name, scope, expiresInDays?}` (`scope`: `read`/`write`, `expiresInDays` 1–3650 or absent for no expiry) | `{"appToken": {…}, "token": "mdp_…"}` |
| DELETE | `/account/tokens/{id}/` | | | 204 |
| GET | `/account/device-links/` | | | `[device link object]`, newest first (the last 24 hours) |
| POST | `/account/device-links/` | yes | `{deviceName?}` | `{"deviceLink": {…}, "token": "mdd_…", "expiresAt": "…", "connectUrl": "…" or null}` |
| GET | `/account/device-links/{id}/` | | | device link object |
| DELETE | `/account/device-links/{id}/` | | | 204 (revoke); 409 `conflict` when already redeemed |

TOTP enrollment: `POST /account/totp/` creates a pending secret (valid 10 minutes, not
active yet; calling it again replaces the pending secret). The client shows the QR code of `otpauthUri` and the `secret` for manual
entry. `POST /account/totp/confirm/` with a current code activates TOTP and returns the
recovery codes, which the client shows once with "copy" and "download" actions. Without
a pending secret (never started or expired), confirm answers 409 `conflict`.

### Admin (admin sessions only)

| Method | Path | Step-up | Request | Response |
|---|---|---|---|---|
| GET | `/admin/users/` | | `?status=active,disabled,deleted` (default: all) | `[{user…, "email", "status", "createdAt", "lastLoginAt", "hasPassword", "totpEnabled", "passkeyCount", "sessionCount"}]` |
| PATCH | `/admin/users/{id}/` | for `role` and `status` | `{role?, status? ("active"/"disabled"), username?, displayName?, color?, email?}` | the user entry |
| DELETE | `/admin/users/{id}/` | yes | | 204 (tombstone) |
| GET | `/admin/users/{id}/sessions/` | | | `[session object]` |
| POST | `/admin/users/{id}/sessions/revoke/` | | | `{"revoked": n}` |
| DELETE | `/admin/sessions/{id}/` | | | 204 |
| POST | `/admin/users/{id}/reset-link/` | yes | `{expiresInMinutes?, resetSecondFactors?}` | `{link, token, url}` |
| GET | `/admin/invites/` | | `?status=pending` (optional filter) | `[link object]` |
| POST | `/admin/invites/` | | `{role, expiresInHours?, note?}` | `{invite: link object, token, url}` |
| DELETE | `/admin/invites/{id}/` | | | 204 (revoke) |
| GET | `/admin/reset-links/` | | `?status=pending` | `[link object]` |
| DELETE | `/admin/reset-links/{id}/` | | | 204 (revoke) |
| GET | `/admin/settings/` | | | `{"require2FA": false}` |
| PATCH | `/admin/settings/` | yes | `{"require2FA": true}` | `{"require2FA": true}` |
| GET | `/admin/audit/` | | `?limit=100&before=<id>&type=login.failed&account=<id>` (`limit` ≤ 500) | `{"events": [{id, time, type, actor: {id, username}?, target: {id, username}?, ip, userAgent, details}], "next": <id or null>}` |

Admin guards (409 `conflict` with an explanation): an admin cannot disable, delete or
demote themselves, and the last active admin cannot be demoted, disabled or deleted.
Disabling or deleting revokes the account's sessions, app tokens and links. Admins
cannot see or change other users' passwords, TOTP or passkeys; the reset link is the
way to help.

### Unchanged

`POST /auth/ws-ticket/` and `GET /auth/whoami/` keep their paths and fields (whoami gains
fields). All content endpoints are unchanged.

## 10. Errors

Every error response is JSON:

```json
{"name": "Unauthorized", "code": "invalid-credentials", "message": "invalid username or password", "details": {}}
```

`name` and `message` exist today; `code` (stable, machine readable) and `details`
(optional object) are new. Clients switch on `code`, never on `message`.

| HTTP | `code` | When |
|---|---|---|
| 400 | `bad-request` | malformed JSON, missing or invalid fields (`details.field`) |
| 400 | `invalid-username` | username rules violated |
| 400 | `weak-password` | `details.reason`: `too-short`, `too-long`, `common`, `repetitive`, `contains-username`; `details.minLength`, `details.maxLength` |
| 400 | `invalid-link` | link token unknown, used, expired or revoked |
| 400 | `invalid-second-factor` | wrong TOTP code during enrollment confirmation |
| 400 | `passkey-registration-failed` | attestation could not be verified |
| 401 | `unauthenticated` | no credentials, or the session/app token is invalid, expired or revoked |
| 401 | `invalid-credentials` | wrong username or password (also unknown user), wrong step-up credentials |
| 401 | `invalid-second-factor` | wrong TOTP or recovery code at login (`details.attemptsLeft`) |
| 401 | `challenge-expired` | login challenge or passkey ceremony expired, used, or out of attempts |
| 401 | `passkey-verification-failed` | passkey assertion rejected |
| 403 | `forbidden` | the role (or app token scope) does not allow this |
| 403 | `account-disabled` | correct credentials, but the account is disabled |
| 403 | `reauth-required` | sensitive action without a recent authentication |
| 403 | `second-factor-setup-required` | restricted session (`require2FA`) |
| 403 | `csrf-check-failed` | missing `X-Requested-With` |
| 403 | `origin-not-allowed` | `Origin` not allowed (or missing with a cookie) |
| 404 | `not-found` | unknown route or object |
| 404 | `passkeys-disabled` | passkeys are not configured |
| 409 | `username-taken` | username (or a reserved former username) exists |
| 409 | `totp-already-enabled`, `totp-not-enabled` | |
| 409 | `last-login-method` | the action would leave the account without password and passkey |
| 409 | `second-factor-required` | the action would leave the account without a second factor while `require2FA` is on |
| 409 | `conflict` | other conflicts (last admin, self-demotion; existing document conflicts keep their old body) |
| 429 | `rate-limited` | `Retry-After` header (seconds) and `details.retryAfter` |
| 500 | `internal` | |

Websocket close codes stay as in `sync-protocol.md`: `4001` unauthenticated or session
ended, `4003` forbidden (foreign actor, role changed).

## 11. Rate limits

In memory, per process (restarts reset them):

- **Per client IP** on the unauthenticated endpoints (`/auth/login/…`,
  `/auth/passkey/login/…`, `/auth/link/…`, `/auth/device-link/…`) and on
  `/auth/reauth/…`: token bucket,
  burst 20, refill 1 per 6 seconds (10 per minute).
- **Per account** (by lower-cased username, also for names that do not exist, so the
  limit does not reveal accounts): after 5 consecutive failed passwords, second factors
  or step-ups, each further attempt must wait 2^(n−5) seconds after the last failure,
  capped at 15 minutes. A success resets the counter. This slows guessing without
  letting an attacker lock the owner out for long.
- **Per login challenge:** 5 attempts.
- Over the limit: 429 `rate-limited` with `Retry-After`. Triggered limits are logged and
  recorded in the audit log (at most once per minute per key).
- The numbers are configurable under `server.auth.rateLimits` (section 13), and
  `rateLimits.disabled: true` turns all of them off. That is meant for automated tests
  that log in many times from one address, never for production.

The client IP is the TCP peer address, unless the peer is in `server.trustedProxies`:
then the backend takes the right-most address of `X-Forwarded-For` that is not a trusted
proxy. `X-Forwarded-Proto: https` from a trusted proxy marks the request as HTTPS
(cookie name and `Secure`, HSTS). Headers from other peers are ignored.

### Security headers

All responses: `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`,
`Referrer-Policy: no-referrer`, `Content-Security-Policy: default-src 'none';
frame-ancestors 'none'` (API responses, never rendered as pages),
`Cross-Origin-Resource-Policy: same-site`. Over HTTPS also
`Strict-Transport-Security: max-age=31536000`. `/auth/…`, `/account/…` and `/admin/…`
responses add `Cache-Control: no-store`.

## 12. Passkeys: RP ID, origins and Android

**RP ID.** A passkey is bound to a domain, the relying party id. It must be the host of
the page that creates or uses the passkey, or a parent domain of it. The web client runs
on `edit.wiki.markusressel.de`, so `backend.wiki.markusressel.de` is not possible (not a
parent), but `wiki.markusressel.de` and `markusressel.de` are. Choose the RP ID once:
changing it later makes all existing passkeys useless.

**The RP ID domain must be reachable from the internet** when the Android app should use
the passkeys. Password managers on Android (Bitwarden, Google Password Manager) do not
read `assetlinks.json` from the phone: Google's Digital Asset Links service fetches it
from `https://<rpId>/.well-known/assetlinks.json`. If that fails, Bitwarden answers the
app with "Passkey not supported for this app", while passkeys in the browser keep working
(the browser checks the origin itself). Check with
`curl "https://digitalassetlinks.googleapis.com/v1/statements:list?source.web.site=https://<rpId>&relation=delegate_permission/common.get_login_creds"`.
A server that is only reachable from a home network therefore needs an RP ID on a public
parent domain that can serve the file.

For Markus' deployment: `rpId: markusressel.de`. The wiki hosts are only reachable at
home, so `wiki.markusressel.de` (the first choice) failed this check. `markusressel.de` is
his blog on Netlify; the file is `public/.well-known/assetlinks.json` in the repository
`markusressel/blog`. The Android builds in use are the debug variant from CI, package
`de.markusressel.mkdocseditor.debug` with the certificate fingerprint
`74:A7:45:FA:C2:4A:BF:BD:F0:A7:30:83:18:71:FE:A6:DE:24:6C:6B:D2:E3:F8:62:49:24:27:1F:C4:D3:70:48`,
so that package goes into `server.auth.webauthn.android` and into the blog's file (the
release package has no published build yet). Default when not configured: the host of
`server.auth.clientUrl`. Without either, passkeys are disabled (`passkeys-disabled`). For
local development `rpId: localhost` works over plain HTTP.

**Origins.** The backend checks the `origin` in `clientDataJSON` against
`server.auth.webauthn.origins` (default: the origin of `clientUrl` plus
`server.cors.allowedOrigins`) and, for each configured Android app, the origin
`android:apk-key-hash:<base64url(SHA-256 of the signing certificate)>`, which the
backend computes from the configured fingerprints. Example: fingerprint
`74:A7:45:FA:…:70:48` gives `android:apk-key-hash:dKdF-sJKv73wpzCDGHH-pt4kbGvS4_hiSSQnH8TTcEg`.

**Android: Digital Asset Links.** Credential Manager only uses passkeys of an RP ID whose
domain vouches for the app. It must serve

```
https://<rpId>/.well-known/assetlinks.json
```

(so `https://markusressel.de/.well-known/assetlinks.json` in Markus' deployment), with status 200, `Content-Type: application/json`, no redirect:

```json
[{
  "relation": ["delegate_permission/common.handle_all_urls", "delegate_permission/common.get_login_creds"],
  "target": {
    "namespace": "android_app",
    "package_name": "de.markusressel.mkdocseditor",
    "sha256_cert_fingerprints": ["<SHA-256 of the release signing certificate, colon-separated hex>"]
  }
}]
```

One entry per package (release, and `de.markusressel.mkdocseditor.debug` with the CI
debug certificate if debug builds should use passkeys). With Play App Signing, use the
fingerprint of Google's app signing key from the Play Console.

The backend can generate this file from `server.auth.webauthn.android` and serve it at
`GET /.well-known/assetlinks.json` (`serveAssetLinks: true`). That only helps when
public requests for the RP ID host reach the backend, for example through an ingress rule
for the path `/.well-known/assetlinks.json` (exact). Otherwise publish the JSON as a
static file on the RP ID host, as Markus' deployment does.

Android **App Links** for the invite, setup, reset and connect links (`https://edit.wiki.…/invite`)
are separate: they need the same file on the **client** host,
`https://edit.wiki.markusressel.de/.well-known/assetlinks.json`, with both relations:
`handle_all_urls` for the App Links and `get_login_creds`, so password managers and
Credential Manager offer the passwords saved for the web client in the app as well
(and the other way round). The web client serves it (`MKDOCSEDITOR_ASSETLINKS_JSON`).
Android verifies App Links through the same Google service, so they also need the
client host to be reachable from the internet. Without App Links, the user opens the link in the browser
(web client) or pastes it into the app; the app reads the token from the fragment.

## 13. Configuration

New keys (all optional):

```yaml
server:
  # Shown by clients in onboarding screens (GET /server-info/). Default: the host of
  # wikiUrl, else of auth.clientUrl, else of the request.
  instanceName: "Markus' Wiki"
  # The published MkDocs site, reported by GET /server-info/.
  wikiUrl: "https://wiki.markusressel.de"
  # The public base URL of this API, used in connect links and /server-info/. Default:
  # derived from the request (scheme from X-Forwarded-Proto of trusted proxies, Host).
  backendUrl: "https://backend.wiki.markusressel.de"
  # Addresses of reverse proxies whose X-Forwarded-For and X-Forwarded-Proto are trusted
  # (CIDRs or single IPs). Default: none, the TCP peer is the client.
  trustedProxies:
    - "10.42.0.0/16"
  auth:
    # Development only: no authentication, every request is the anonymous admin.
    disabled: false
    # The web client, used to build setup, invite, reset and connect links, reported as
    # webClientUrl by /server-info/, and the default passkey origin and RP ID.
    clientUrl: "https://edit.wiki.markusressel.de"
    # Accept HTTP Basic with username + password (accounts without a second factor). Only
    # for the transition from server.auth.users. Default: false. Username + app token
    # works without it.
    legacyBasicAuth: false
    # Session lifetimes (Go durations).
    sessionIdleTimeout: "720h"   # 30 days
    sessionMaxLifetime: "2160h"  # 90 days
    # Secure flag of the session cookie: "auto" (when the request is HTTPS, directly or
    # through a trusted proxy), "always", "never". Default: auto.
    cookieSecure: "auto"
    # File with the 32-byte server key (base64) that encrypts TOTP secrets and keys the
    # recovery code hashes. Default: <stateDir>/auth.key, created with mode 0600 on first
    # start. The environment variable MKDOCSREST_AUTH_SECRET_KEY (base64) takes precedence.
    secretKeyFile: ""
    # How long audit events are kept. Default: 8760h (365 days).
    auditRetention: "8760h"
    # Rate limits of section 11. Defaults shown. disabled: true is for automated tests only.
    rateLimits:
      disabled: false
      perIpBurst: 20
      perIpRefill: "6s"
      accountFailuresBeforeBackoff: 5
      maxBackoff: "15m"
    webauthn:
      # Relying party id, see docs/auth.md section 12. Default: host of clientUrl.
      rpId: "markusressel.de"
      # Name shown by authenticators. Default: MkDocsEditor
      rpName: "MkDocsEditor"
      # Allowed web origins. Default: origin of clientUrl + server.cors.allowedOrigins
      origins:
        - "https://edit.wiki.markusressel.de"
      # Android apps that may use the passkeys. Adds their android:apk-key-hash origins
      # and the entries of /.well-known/assetlinks.json.
      android:
        - packageName: "de.markusressel.mkdocseditor"
          sha256CertFingerprints:
            - "AB:CD:…"
      # Serve /.well-known/assetlinks.json from the backend. Default: false
      serveAssetLinks: false
    # Only read once, to import existing users into the database (section 14).
    users: []
```

`server.basicAuth` (the old single user) is also only read for the import.

For local development, with the web client on `http://localhost:4174` and the backend
on `http://localhost:17414`: `server.cors.allowedOrigins: ["http://localhost:4174"]`,
`server.auth.clientUrl: "http://localhost:4174"`, `server.auth.webauthn.rpId: "localhost"`.
Both are same-site, the cookie is `mkdocseditor_session` without `Secure`, and browsers
allow WebAuthn on `localhost` over plain HTTP. Use `localhost` in both URLs, not
`127.0.0.1` for one of them: an IP address and `localhost` are different sites.

**Server key.** Losing `auth.key` makes TOTP secrets and recovery codes unreadable;
affected users then need a reset link with `resetSecondFactors`. The key is not needed
to verify passwords, sessions or passkeys. Keep it next to the database in backups, or
provide it through the environment (a Kubernetes secret).

## 14. Migration from configured users

The upgrade must not lock anyone out.

On start, after opening `auth.db`:

1. **Import.** If the database has no accounts and the configuration has
   `server.auth.users` or `server.basicAuth`, every configured user becomes an account
   with the same username, display name, email, role and colour, and the bcrypt hash as
   password (upgraded to Argon2id at the first login). The `basicAuth` user becomes an
   admin; its plain-text password is hashed with Argon2id right away. Each import is
   logged and recorded as `account.imported`. The import happens once: the database
   records it, and from then on `server.auth.users` and `server.basicAuth` are
   **ignored**; the server logs a warning on every start while they are still in the
   file. Remove them after the upgrade. Passwords that do not meet the new rules keep
   working until the user changes them.
2. **Actor bindings.** If `<stateDir>/actors.json` exists and has not been imported, every
   binding moves into the database: usernames map to the account ids, `:disk` and
   `:server` stay pseudo users, `anonymous` stays `anonymous`. A username without an
   account (a user removed from the configuration earlier) becomes a **deleted** account
   (tombstone) with that username, so its authorship still resolves. The file is then
   renamed to `actors.json.imported` and never read again (it is kept, not deleted).
3. **Git.** Pending edit counts in `git-pending.json` that are keyed by usernames are
   mapped to account ids when loaded. Commit authors keep the format
   `Display Name <email or username@mkdocseditor.invalid>`; history resolves
   `username@mkdocseditor.invalid` through current and former usernames.
4. **Without configured users** (and not `auth.disabled`): setup mode, see section 7.
   Before, such a server ran without authentication; it now requires the setup link. To
   keep the old behaviour for development, set `server.auth.disabled: true`.

**Transition for clients.** `legacyBasicAuth` defaults to `false`. Clients older than the
account system (HTTP Basic with the configured passwords) only keep working when the
operator sets `server.auth.legacyBasicAuth: true`; the imported accounts keep the same
usernames and passwords. Once all clients use sessions, remove the flag.

Steps for a running deployment:

1. Back up the state directory.
2. Deploy the new version with the old configuration plus `server.auth.clientUrl`,
   `server.trustedProxies`, `server.auth.webauthn.*` and, only while old clients are
   still in use, `server.auth.legacyBasicAuth: true`.
3. Check the log for the import lines. Log in with the web client using the existing
   passwords; set up passkeys or TOTP.
4. After the clients are updated: remove `server.auth.users`, `server.basicAuth` and
   `legacyBasicAuth` from the configuration.

## 15. Changes to protocol v1

Additive only; existing clients keep working.

- `ActorInfo` (`GET /actors/`, the `actor` message, `initial-content.actors`),
  `UserInfo` (`you`, presence, `online`) and authorship runs gain `accountId` (empty for
  `:disk`, `:server`). `username` stays and is the **current** username of the account,
  so it can change; clients that store authorship should key on `accountId`.
- History authors (`/document/{id}/history/`) gain `accountId` (empty when the author is
  not an account).
- Section 3 of `sync-protocol.md` is replaced by sections 4–6 of this document: the
  websocket upgrade additionally accepts the session cookie and bearer tokens; the
  ticket flow is unchanged.
- REST error bodies gain `code` and `details`.

## 16. What clients must implement

**Both:**

- `GET /auth/status/` on start: `setupRequired` → setup screen (token from the setup
  link or pasted), `passkeys.enabled` → offer passkeys, `passwordPolicy` → form checks.
- Login: password form → session or second-factor step (TOTP field, "use a recovery
  code", "use a passkey"); passkey button (discoverable, no username).
- Link handling for `/setup`, `/invite`, `/reset` with the token from the fragment:
  inspect, then a form for username, display name and password, or "create a passkey".
- Handle `reauth-required` on sensitive actions: prompt for password (+ TOTP code) or
  passkey, call `/auth/reauth/…`, then retry the action once.
- Handle `second-factor-setup-required`: route to the 2FA setup screen.
- Handle 401 `unauthenticated`: drop local session state and show the login.
- Handle 429 with `Retry-After`.
- Account screens: profile (display name, colour, git email), password change, TOTP
  enroll (QR from `otpauthUri`, secret for manual entry, confirm, show recovery codes
  once), recovery codes regenerate, passkeys (list, add, rename, delete), sessions (list,
  revoke, revoke others), app tokens (optional).
- Admin screens (role `admin`): users (role, disable/enable, delete, reset link with QR
  code, sessions), invites (create with role, expiry, note; show link and QR code; list;
  revoke), reset links (list, revoke), settings (`require2FA`), audit log (paged with
  `before`).
- Use `accountId` instead of `username` as the key for authorship and actors.
- Device pairing: the web client offers "connect the Android app" (step-up, then
  `POST /account/device-links/`, QR code of `connectUrl`, poll
  `GET /account/device-links/{id}/` every few seconds until `redeemed` or `expired`,
  revoke on cancel); Android handles `/connect#…`.

**Web:**

- `fetch` with `credentials: "include"` and `X-Requested-With: XMLHttpRequest` on every
  request; `sessionType: "cookie"`. Never store tokens in `localStorage`.
- Websockets: rely on the cookie, or keep using tickets (both work).
- Passkeys: pass `publicKey` to `@simplewebauthn/browser` (`startRegistration`,
  `startAuthentication`, conditional UI with `useBrowserAutofill`), send the returned
  JSON as `credential`.

**Android:**

- `sessionType: "bearer"`, a meaningful `deviceName`, store the token in encrypted
  storage (Android Keystore), send `Authorization: Bearer mds_…` on every request and on
  the websocket upgrade. Send `X-Requested-With` on the unauthenticated `POST` requests
  (login, links, passkey login), since they carry no `Authorization` header.
- Passkeys via Credential Manager with the `publicKey` JSON; send
  `registrationResponseJson` / `authenticationResponseJson` as `credential`.
- Optional App Links for the client host's `/setup`, `/invite`, `/reset` and `/connect`
  paths (needs `assetlinks.json` on that host, section 12).
- Onboarding: `GET /server-info/` for the instance name, protocol version and login
  methods. Connect links: decode the payload, check `v` and `expiresAt`, show the
  instance name and username, then redeem with a meaningful `deviceName`.

## 17. Threat model

**Assets:** wiki content and its history, account credentials, the ability to act as
another user (authorship, git commits), the admin role.

**Attackers considered:** someone on the internet or LAN without an account; a signed-in
user trying to exceed their role; a malicious website visited by a signed-in user; an
attacker who obtains a copy of the database or a backup; an attacker who obtains logs.
Out of scope: an attacker with code execution on the server or root on the host, a
compromised client device while unlocked, a malicious admin.

| Threat | Mitigation |
|---|---|
| Password guessing, credential stuffing | Argon2id; blocklist of common passwords; per-IP and per-account rate limits with exponential backoff; optional TOTP; passkeys are phishing-resistant; generic errors |
| User enumeration | same error and dummy hash for unknown users; per-username backoff also for non-existent names; link inspect needs a valid token |
| Stolen database or backup | passwords Argon2id; session, app, link and challenge tokens stored as SHA-256 of 256-bit randoms; TOTP secrets AES-GCM encrypted and recovery codes HMAC'd with a key outside the database; passkeys store only public keys |
| Stolen log | tokens never in URLs (fragments, bodies); the setup token is logged on purpose, is single use, expires in 24 h and only works while no account exists |
| Session theft (XSS in the web client) | HttpOnly cookie; the web client never sees the token; CSP is the web client's job |
| Session fixation | a token is only created after complete authentication; no pre-auth session exists |
| CSRF, login CSRF | required `X-Requested-With` (forces a CORS preflight), Origin allowlist, SameSite=Lax |
| Cross-site websocket hijacking | the cookie is only accepted on upgrades from allowed origins; tickets are single use and short-lived |
| Malicious website reading API responses | CORS credentials only for configured origins |
| Forged client IP to evade rate limits | `X-Forwarded-For` only from `trustedProxies`, right-most untrusted address |
| Lost device | sessions listed and revocable per device; admin can revoke all sessions; reset link with `resetSecondFactors` |
| Phishing of TOTP codes | passkeys recommended; TOTP replay protection; codes valid ±30 s |
| Privilege escalation | role checked on every request from the database; reader websockets never apply changes; admin endpoints only with admin sessions, not app tokens; last-admin guard |
| Stale privileges after role change or disable | role read per request; websockets closed on change; disable/delete revoke sessions and tokens |
| Invite or reset link leaked | single use, expiry, revocable, audit-logged; reset links default to 1 h; redeeming a reset revokes all sessions |
| Impersonation in authorship | actor ids bound to account ids on first use (protocol v1); tombstones keep old authors resolvable; former usernames stay reserved |
| Memory exhaustion through hashing | at most 4 concurrent Argon2id computations; rate limits |
| Timing attacks on secrets | lookups by hash, `subtle.ConstantTimeCompare` for remaining comparisons |
| Brute force of link or session tokens | 256-bit tokens; per-IP limit on link endpoints |
| Device link QR code photographed or link forwarded | creation needs step-up; 5-minute, single-use token; the web shows which device redeemed it; the session is listed and revocable; the new session needs its own step-up for sensitive actions |

Residual risks: rate limits are per process and reset on restart; an attacker with read
access to both the database and `auth.key` can read TOTP secrets (not passwords);
`legacyBasicAuth` sends passwords on every request and bypasses 2FA, so it is off by
default and meant for the transition only; `auth.disabled` is for development only.
