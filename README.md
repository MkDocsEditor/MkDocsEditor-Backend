# MkDocsEditor-Backend

Server backend for MkDocsEditor clients.

## How to use

### Configuration

Create a `mkdocsrest.yaml` file similar to the [mkdocsrest_example.yaml](mkdocsrest_example.yaml).

### Users and authentication

Every request except `GET /alive/` needs HTTP basic auth against the users in
`server.auth.users`. Each user has a bcrypt password hash and a role:

| Role | Can |
|---|---|
| `admin` | everything |
| `editor` | read and change documents, sections and resources |
| `reader` | read everything and follow live edits, change nothing |

Create a password hash with the backend itself (it asks for the password twice):

```bash
mkdocsrest hash-password
# or with Docker
docker run --rm -it ghcr.io/mkdocseditor/mkdocseditor-backend:latest /app/main hash-password
# or non-interactively
printf '%s' 'the password' | mkdocsrest hash-password
```

`htpasswd -nbB user password` also works (use the part after the colon).

The old single-user configuration (`server.basicAuth.user` and `password`) still works;
that user is an admin. Without any user, authentication is off and the backend warns at
startup.

Clients:

- `GET /auth/whoami/` returns the authenticated user (`username`, `displayName`, `role`,
  `color`).
- Websockets: Android sends the basic auth header on the upgrade request. Browsers cannot
  set headers on a websocket, so they get a single-use ticket, valid for 60 seconds, from
  `POST /auth/ws-ticket/` and send it in the first websocket message. See
  [docs/sync-protocol.md](docs/sync-protocol.md#3-authentication).
- Browser clients on another origin must be listed in `server.cors.allowedOrigins`.
  Scripts should send `X-Requested-With: XMLHttpRequest`, so a 401 does not make the
  browser show its own login dialog.

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
