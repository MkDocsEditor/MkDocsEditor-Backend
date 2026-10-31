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

### General

| Method | Path           | Description                                  |
|--------|----------------|----------------------------------------------|
| GET    | /alive         | Liveness probe endpoint                      |
| GET    | /mkdocs/config | Retrieve the MkDocs configuration (`mkdocs.yml`) |
| GET    | /auth/whoami   | The authenticated user                       |
| POST   | /auth/ws-ticket | A single-use websocket ticket (60 s)        |

### Sections

| Method | Path                 | Description                                           |
|--------|----------------------|-------------------------------------------------------|
| GET    | /section             | Retrieve the whole section tree                       |
| GET    | /section/<sectionId> | Retrieve the section with the given `sectionId`       |
| POST   | /section             | Create a new section                                  |
| PUT    | /section/<sectionId> | Update an existing section with the given `sectionId` |
| DELETE | /section/<sectionId> | Delete the section with the given `sectionId`         |

### Documents

| Method | Path                           | Description                                                                                                 |
|--------|:-------------------------------|-------------------------------------------------------------------------------------------------------------|
| GET    | /document/<documentId>         | Retrieve the document with the given `documentId`                                                           |
| GET    | /document/<documentId>/ws      | Websocket endpoint for realtime communication regarding updates of the document with the given `documentId` |
| GET    | /document/<documentId>/content | Retrieve the current content of the document with the given `documentId`                                    |
| POST   | /document                      | Create a new document                                                                                       |
| PUT    | /document/<documentId>         | Rename an existing document with the given `documentId`                                                     |
| DELETE | /document/<documentId>         | Delete the document with the given `documentId`                                                             |

### Resources

| Method | Path                           | Description                                                              |
|--------|--------------------------------|--------------------------------------------------------------------------|
| GET    | /resource/<resourceId>         | Retrieve the resource with the given `resourceId`                        |
| GET    | /resource/<resourceId>/content | Retrieve the current content of the resource with the given `resourceId` |
| POST   | /resource                      | Upload a new resource                                                    |
| PUT    | /resource/<resourceId>         | Rename an existing resource with the given `resourceId`                  |
| DELETE | /resource/<resourceId>         | Delete the resource with the given `resourceId`                          |

# Contributing

GitHub is for social coding: if you want to write code, I encourage
contributions through pull requests from forks of this repository.
Create GitHub tickets for bugs and new features and comment on the ones
that you are interested in.

# License

AGPLv3+
