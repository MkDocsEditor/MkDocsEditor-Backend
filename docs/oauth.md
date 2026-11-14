# OAuth for MCP clients

MCP clients such as Claude Code can connect to the MCP server ([mcp.md](mcp.md)) without an
app token copied by hand. They follow the authorization part of the MCP specification, which
is OAuth 2.1:
1. The client asks `/api/mcp` without a token and gets `401`.
2. The client reads the metadata and registers itself.
3. It opens the browser, the user approves in the web app, and the client gets tokens.

Each approval creates a **grant**: an app token of the account, named after the client. It
appears in Settings, App tokens and is revoked there like any other app token.

The backend is both the authorization server and the protected resource:

| | Value |
|---|---|
| issuer | the backend URL: `server.backendUrl`, else `<origin>/api` |
| resource (RFC 8707) | `<backend URL>/mcp`, e.g. `https://wiki.example.com/api/mcp` |
| scopes | `read` (the read tools), `write` (all tools); `write` includes `read` |

Without accounts (`server.auth.disabled`) every OAuth endpoint answers `404`.

## 1. Flow

1. `POST /api/mcp` without a token answers `401` with
   `WWW-Authenticate: Bearer resource_metadata="<backend URL>/.well-known/oauth-protected-resource/mcp", scope="read write"`.
   A token that is wrong or expired also gets `error="invalid_token"`, so the client refreshes
   it or starts over.
2. The protected resource metadata (RFC 9728) names the issuer. Its metadata (RFC 8414) has the
   endpoints.
3. `POST /api/oauth/register` (RFC 7591) registers the client.
4. The client opens `GET /api/oauth/authorize?response_type=code&client_id=…&redirect_uri=…&code_challenge=…&code_challenge_method=S256&state=…&scope=…&resource=…`
   in the browser. The backend checks the request and redirects to the web app:
   `<auth.clientUrl>/connect/authorize?request=<id>`. Without `auth.clientUrl` it uses the
   origin of the request.
5. The web app logs the user in if needed and shows the request (`GET /api/oauth/requests/{id}/`).
   The user approves or denies. The answer has a `redirectUrl`, and the web app sends the
   browser there: the client's redirect URI with `code` and `state`, or with
   `error=access_denied`.
6. The client trades the code for tokens at `POST /api/oauth/token`, with its PKCE
   `code_verifier`.
7. The client calls `/api/mcp` with `Authorization: Bearer mdo_…`. When the access token
   expires, it gets new tokens with its refresh token.

Pending requests (10 minutes) and codes (1 minute) live in memory. After a restart of the
backend the client starts again at step 4.

## 2. Endpoints

All OAuth endpoints are public. They ignore session cookies and `Authorization` headers, and
need no CSRF headers.

The register, authorize, token and revoke endpoints share a rate limit per address: a burst
of 30, then one request every 2 seconds. The consent endpoints need a session.

### Metadata

The paths are below `/api`. The root paths (outside `/api`) are where clients look first.

| Path | Content |
|---|---|
| `/.well-known/oauth-protected-resource`, `/.well-known/oauth-protected-resource/api/mcp` (root); `/api/.well-known/oauth-protected-resource`, `/api/.well-known/oauth-protected-resource/mcp` | protected resource metadata |
| `/.well-known/oauth-authorization-server`, `/.well-known/oauth-authorization-server/api`, `/.well-known/openid-configuration`, `/.well-known/openid-configuration/api` (root); `/api/.well-known/oauth-authorization-server`, `/api/.well-known/openid-configuration` | authorization server metadata |

Clients that follow `resource_metadata` of the `401` find everything below `/api`:
- the protected resource metadata at `/api/.well-known/oauth-protected-resource/mcp`
- the authorization server metadata of the issuer `<origin>/api` at
  `/api/.well-known/openid-configuration`

So a reverse proxy that only forwards `/api` is enough for them. Clients that guess the
metadata location without following the header need the root paths: forward
`/.well-known/oauth-protected-resource`, `/.well-known/oauth-authorization-server` and
`/.well-known/openid-configuration` (with everything below them) to the backend too.

The authorization server metadata has:
- `response_types_supported: ["code"]`
- `grant_types_supported: ["authorization_code", "refresh_token"]`
- `code_challenge_methods_supported: ["S256"]`
- `token_endpoint_auth_methods_supported: ["none"]`
- `client_id_metadata_document_supported: false`

Clients register; client ID metadata documents are not supported.

### `POST /api/oauth/register`

JSON with `redirect_uris` (1 to 10), optionally `client_name` (up to 100 characters; the
name of the grant, default `MCP client`), `grant_types` and `response_types`.

The answer is `201` with `client_id` and `token_endpoint_auth_method: "none"`. All clients
are public: they have no secret and prove themselves with PKCE.

Redirect URIs:
- `https://…`
- `http://` only on the loopback interface: `127.0.0.1`, `[::1]`, `localhost`. Any port
  matches later (RFC 8252 section 7.3), but the path must be the same.
- private-use schemes of native apps (`cursor://…`), except schemes a browser could be abused
  with: `javascript:`, `data:`, `vbscript:`, `file:`, `blob:` and others.
- no fragments, no user info

Other redirect URIs match exactly, as strings.

Clients that never got a token are deleted after 24 hours.

### `GET /api/oauth/authorize`

Requires:
- `response_type=code`
- a registered `client_id` and `redirect_uri`
- `code_challenge` with `code_challenge_method=S256`; `plain` is not accepted

`scope` is `read`, `write` or `read write`. Unknown scopes are ignored; without any known
scope the default is `write`. `resource` must be the MCP endpoint if given.

An unknown client or redirect URI gets a plain `400` page, never a redirect. Other errors
redirect to the client with `error`, `error_description` and `state`:
- `unsupported_response_type`
- `invalid_request`
- `invalid_scope`
- `invalid_target`

### Consent: `/api/oauth/requests/{id}/`

For the web app; sessions only, app tokens get `403`.

- `GET /api/oauth/requests/{id}/` returns the request:
  - `id`, `clientId`
  - `clientName`: chosen by the client itself. Show it as a claim, next to the host of
    `redirectUri`.
  - `redirectUri`
  - `scope`: the requested scope, `read` or `write`
  - `allowedScopes`: `["read"]` for readers, else `["read", "write"]`
  - `resource`, `expiresAt`

  Unknown, decided and expired requests are `404`.
- `POST /api/oauth/requests/{id}/approve/` with an optional body `{"scope": "read"}` lowers
  the scope; readers always get `read`. Like creating an app token, it needs a recent login:
  `403 reauth-required`, then `POST /api/auth/reauth/` and approve again. The answer is
  `{"redirectUrl": "…"}` with `code` and `state`.
- `POST /api/oauth/requests/{id}/deny/` answers `{"redirectUrl": "…"}` with
  `error=access_denied` and `state`.

A request is decided once. `features` of `GET /api/server-info/` contains `oauth` when these
endpoints exist.

### `POST /api/oauth/token`

Form-encoded (`application/x-www-form-urlencoded`). `client_id` is in the form, or the user
of Basic auth.

- `grant_type=authorization_code` with `code`, `redirect_uri`, `client_id`, `code_verifier`
  and optionally `resource`.
- `grant_type=refresh_token` with `refresh_token`, `client_id` and optionally `scope=read`
  to lower the scope.

The answer has `access_token`, `token_type: "Bearer"`, `expires_in: 3600`, `refresh_token`
and `scope` (`read`, or `read write`). It has `Cache-Control: no-store`.

Errors follow RFC 6749 section 5.2: `{"error": "invalid_grant", "error_description": "…"}`
with `400`, or `401` for `invalid_client`.

### `POST /api/oauth/revoke`

RFC 7009, form-encoded `token`: an access or refresh token. Ends the whole grant. Unknown
tokens are no error.

## 3. Tokens and grants

| Token | Format | Lifetime |
|---|---|---|
| code | `mdk_` + 43 base64url characters | 1 minute, one use |
| access token | `mdo_` + 43 base64url characters | 1 hour |
| refresh token | `mdr_` + 43 base64url characters | 30 days, one use |

- Access tokens only work at `/api/mcp`, the resource they were issued for. Everywhere else
  they get `403`.
- The grant's rights are the intersection of its scope and the account's role, as for app
  tokens. A change of the role applies at once.
- Edits made with an access token are attributed like those of an app token: to the
  account, with the grant's name as the agent (see [content-api.md](content-api.md) and
  [mcp.md](mcp.md)).
- A refresh returns a new access and refresh token. The old access token ends, and the grant
  lasts another 30 days. The app token's `expiresAt` shows when it ends without a refresh.
- **Replays end the grant:**
  - a code that is used a second time
  - a refresh token that is used a second time

  Each revokes the grant made with it, because the token may have been stolen.
- A grant ends when it is revoked as an app token, with `POST /api/oauth/revoke`, or with
  every app token of the account: password change, reset, disabling, deleting.

In `GET /api/account/tokens/`, grants are app tokens with:
- `oauthClientId`
- `resource`

Their `mdp_` token is never handed out.

## 4. Audit log

| Event | When |
|---|---|
| `oauth.client-registered` | a client registered (with name and redirect URIs, no account) |
| `oauth.approved` | the user approved a request (client, scope, redirect URI) |
| `oauth.denied` | the user denied a request |
| `app-token.created` | the client got its first tokens; details have `oauthClientId` |
| `app-token.revoked` | the grant ended; `reason` is `code-reused`, `refresh-token-reused` or `client` for the revocation endpoint |

## 5. Security notes

- Redirect URIs match exactly. The only exception is the port of loopback URIs. The web
  app navigates to `redirectUrl` only after the backend built it from a registered URI, so
  it never goes to a URI the client did not register.
- PKCE S256 is required. Codes are single-use and live a minute.
- Registration needs no account. The rate limit and the 24-hour cleanup bound what an
  anonymous caller can store.
- The consent screen shows names the client chose. The redirect URI host is what tells a
  user where the code goes.
