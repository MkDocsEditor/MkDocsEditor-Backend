# MkDocsEditor-Backend

Server backend for MkDocsEditor clients.

## How to use

### Configuration

Create a `mkdocsrest.yaml` file similar to the [mkdocsrest_example.yaml](mkdocsrest_example.yaml).

### Users and authentication

Accounts live in an SQLite database in the state directory (`auth.db`, see
[docs/auth.md](docs/auth.md) for the full design). Every request except `GET /alive/`,
`GET /auth/status/` and the login and link endpoints needs a signed-in user.

**First admin.** Without any account, the backend starts in setup mode and logs a setup
link (or, without `server.auth.clientUrl`, a setup token). Open it in the web client to
create the first admin. `mkdocsrest setup-link` prints a fresh link, for example with
`kubectl exec`; it replaces the previous one.

**More users** join with invite links that an admin creates (`POST /admin/invites/`):
role, expiry, single use and a note, revocable until used. There is no email: a
forgotten password is reset with a reset link from an admin, and the reset ends all
sessions of that account. Admins manage accounts under `/admin/` (roles, disabling,
deletion as a tombstone that keeps authorship, sessions, reset links, the instance-wide
`require2FA` setting and the audit log).

**Connecting the Android app:** a signed-in user creates a device link in the web client
(`POST /account/device-links/`) and scans its QR code with the app. The single-use link is
valid for 5 minutes and signs the app in without typing the server address or the
password. `GET /server-info/` describes the instance for onboarding screens.

**Scripts** use app tokens (`POST /account/tokens/`, scope `read` or `write`) as
`Authorization: Bearer mdp_…`, or as the password of HTTP basic auth.

| Role | Can |
|---|---|
| `admin` | everything, including user administration |
| `editor` | read and change documents, sections and resources |
| `reader` | read everything and follow live edits, change nothing |

With `server.auth.clientUrl` (or `server.auth.webauthn.rpId`) set, users can register and
log in with passkeys from any provider (Bitwarden, Google Password Manager, iCloud
Keychain and others), also from the Android app; see
[docs/auth.md](docs/auth.md#12-passkeys-rp-id-origins-and-android) for the RP ID and
the `assetlinks.json` the app needs.

Passwords need at least 10 characters and must not be on a list of common passwords.
They are stored as Argon2id hashes. Users can add TOTP (any authenticator app or password
manager) with 10 single-use recovery codes. Sensitive changes ask for the password again
when the last login is older than 10 minutes.

**Back up `auth.key` with `auth.db`.** The server key in the state directory (or in
`MKDOCSREST_AUTH_SECRET_KEY`) encrypts the TOTP secrets; without it, users with TOTP need
a reset link.

**Upgrading from configured users.** On the first start of this version, the users of
`server.auth.users` (and the old `server.basicAuth` user) are imported into the database
once, with their roles and bcrypt hashes; a bcrypt hash is replaced by Argon2id on the
next login. After that the configuration list is ignored and can be removed. HTTP basic
auth with these passwords is off by default; set `server.auth.legacyBasicAuth: true`
while old clients are still in use. `mkdocsrest hash-password` still creates bcrypt
hashes for that list.

Clients:

- The web client logs in with `POST /auth/login/` and gets an HttpOnly session cookie.
  Requests that change something send `X-Requested-With: XMLHttpRequest`, and their
  `Origin` must be the backend itself or listed in `server.cors.allowedOrigins`.
- Native clients ask for a bearer session (`"sessionType": "bearer"`) and send
  `Authorization: Bearer mds_…`.
- `GET /auth/whoami/` returns the signed-in user (`id`, `username`, `displayName`,
  `role`, `color`).
- Websockets: the session cookie or bearer header on the upgrade request, or a
  single-use ticket from `POST /auth/ws-ticket/` in the first message. See
  [docs/sync-protocol.md](docs/sync-protocol.md#3-authentication). Ending a session
  closes its websockets.
- Behind a reverse proxy, list it in `server.trustedProxies`, so rate limits and the
  audit log see the real client address and the cookie gets the `Secure` flag.

### Realtime editing state

Documents are edited live with [Automerge](https://automerge.org). The backend keeps the
Automerge state of every document it has opened in `mkdocs.stateDir` (default
`<projectPath>/.mkdocseditor/`). The folder must be writable and outside the docs
folder, and it must survive restarts. It is what lets offline edits of the clients merge
later. If the project is a git repository, the backend adds the folder to
`.git/info/exclude`. The markdown files stay the source of truth for MkDocs: the backend
writes every change to them within two seconds, and merges changes made on disk (git
pull, manual edits) into the live documents.

### Git commits

With `git.enabled: true` the backend commits the changes made through it to the git
repository that holds the docs folder (see `mkdocsrest_example.yaml` for the settings).
Edits are batched per document: a commit is made when all users have left the document
for 5 minutes, after 10 minutes without a change, or at least every 60 minutes during
continuous editing, and on shutdown. The author is the user with the most changes, the
other contributors are added as `Co-authored-by:` trailers, and the backend is the
committer. Creating, renaming, deleting and uploading over the API is committed right
away. The backend commits only the files it changed, so other uncommitted or staged work
in the repository stays untouched, and changes made on disk (for example by `git pull`)
are never committed by it. `git.push: true` pushes after each commit. The repository may
belong to another user (an NFS volume), git's ownership check is skipped for it.

### Docker

Run the service using Docker and mount the configuration file and the wiki folder:

```bash
docker run -d \
    -p 7413:7413 \
    -v ~/mkdocsrest.yaml:/app/mkdocsrest.yaml \
    -v ~/mywiki:/data \
    ghcr.io/mkdocseditor/mkdocseditor-backend:latest
```

The container runs as uid/gid 1000, so the wiki folder must be writable by that user
(`chown -R 1000:1000 ~/mywiki`, or run the container with `--user` matching the owner).

### Connect

Use a client to connect to the service.

## Clients

- [MkDocsEditor-Android](https://github.com/MkDocsEditor/MkDocsEditor-Android)
- [MkDocsEditor-Web](https://github.com/MkDocsEditor/MkDocsEditor-Web)

## API

The realtime sync protocol (websocket, Automerge, presence, authorship, history) and the
storage and git architecture are specified in [docs/sync-protocol.md](docs/sync-protocol.md).
References between pages, moving and renaming with automatic link updates, and the link
path algorithm the clients share are specified in [docs/references.md](docs/references.md).

[OpenAPI Documentation](https://editor-next.swagger.io/?url=https://raw.githubusercontent.com/MkDocsEditor/MkDocsEditor-Backend/master/openapi-description.yaml)

The [OpenAPI file](openapi-description.yaml) is the reference for the REST API. CI lints it and
checks that it lists exactly the routes the backend serves. Overview:

| Method | Path | Description |
|---|---|---|
| GET | `/alive/` | Liveness probe, no authentication |
| GET | `/auth/whoami/` | The authenticated user |
| POST | `/auth/ws-ticket/` | A single-use websocket ticket (60 s) |
| GET | `/actors/` | Automerge actor id to user map |
| GET | `/mkdocs/config/` | The MkDocs configuration (`mkdocs.yml`) as JSON |
| GET | `/section/` | The whole tree |
| POST | `/section/` | Create a section (`{"parent","name"}`) |
| GET, PUT, DELETE | `/section/{id}/` | Get, rename (`{"name"}`), delete (`?force=true` while documents in it are open) |
| POST | `/document/` | Create a document (`{"parent","name"}`) |
| GET, PUT, DELETE | `/document/{id}/` | Get, rename (`{"name"}`), delete (`?force=true` while it is open) |
| GET | `/document/{id}/content/` | The markdown text (live while open) |
| GET | `/document/{id}/authorship/` | Who wrote which part of the text |
| GET | `/document/{id}/references/` | Outgoing links, images and other references, resolved or broken |
| GET | `/document/{id}/backlinks/`, `/section/{id}/backlinks/`, `/resource/{id}/backlinks/` | References to the item ("used by") |
| GET | `/references/broken/` | All broken references |
| GET | `/references/unused-resources/` | Resources that nothing uses |
| GET | `/link/?from=&to=&anchor=` | The link destination from a document to an item, see [docs/references.md](docs/references.md#8-the-link-path-algorithm) |
| GET | `/document/{id}/history/` | Git commits of the document (requires `git.enabled`) |
| GET | `/document/{id}/history/{revision}/content/` | The text at a revision |
| GET | `/document/{id}/history/diff/?from=&to=` | Unified diff between revisions (`current` is the live text) |
| POST | `/document/{id}/history/{revision}/restore/` | Restore a revision as a normal edit |
| GET | `/events/ws/` | Events websocket: tree changes and who has which document open |
| GET | `/document/{id}/ws/` | Realtime editing websocket, see [docs/sync-protocol.md](docs/sync-protocol.md) |
| POST | `/resource/{sectionId}/{name}/` | Upload a file (multipart field `file`) |
| GET, PUT, DELETE | `/resource/{id}/` | Get, rename (`{"name"}`), delete |
| GET | `/resource/{id}/content/` | The file |

# Contributing

GitHub is for social coding: if you want to write code, I encourage
contributions through pull requests from forks of this repository.
Create GitHub tickets for bugs and new features and comment on the ones
that you are interested in.

# License

AGPLv3+
