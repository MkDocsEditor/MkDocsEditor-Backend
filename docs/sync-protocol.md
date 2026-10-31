# MkDocsEditor sync protocol and architecture

This document is the contract between the backend and the clients (web, Android) for
realtime collaborative editing. It covers authentication, the document websocket
(Automerge sync, presence, authorship), revision history, and how the backend stores
documents and commits them to git.

Status: **version 1**. Implemented: authentication, roles, the document websocket,
presence, persistence, external changes, readers, rename and delete. Not yet
implemented: authorship (section 7), git integration (section 10), revision history
(section 11). The REST API reference is
[`openapi-description.yaml`](../openapi-description.yaml). Where the two disagree on a
REST endpoint, the OpenAPI file wins. For the websocket protocol, this document wins.

Target feature set (modelled on CodiMD):

- realtime co-editing of one markdown file by several users
- cursors and selections of the other users, with their names and colours
- a list of the users currently in a document
- per-user authorship ("who wrote what")
- revision history with diff and restore
- roles: admin, editor, reader

## Guarantee: edits are never lost

Edits must never be lost. That includes edits made offline, edits made while the server
restarts, and edits that race with external changes on disk. This is the reason for
using Automerge. Every part of this design follows from it:

- The server never truncates, resets or compacts the Automerge history of a document.
  It starts a new epoch only when its saved state is gone (see
  [Epochs](#epochs-and-offline-edits)), and even then the client re-applies its
  unsynced edits.
- The server never writes a file, and never commits one, without first merging the
  file's current disk content into the document (see [Disk writes](#disk-writes)).
- Clients persist their document and the heads the server is known to have, so they
  can merge offline edits after any reconnect, even into a new epoch.

An integration test in CI checks this. Client A loads a document and goes offline, then
edits. Meanwhile client B edits, the server restarts, and the file changes on disk. When
A reconnects, all three edits must be present on A, on B, in the server document and on
disk.

## 1. Versions

| Component | Library | Automerge core |
|---|---|---|
| Backend | `github.com/automerge/automerge-go` v0.0.0-20241030180337-6fb4f2d08244 (latest, no tagged release) | automerge-rs 0.5.0 |
| Web | `@automerge/automerge` 3.5.0 (npm) | current |
| Android | `org.automerge:automerge` (automerge-java) 0.0.9 | current |

All of them speak the same binary document format and sync protocol (Automerge 2
columnar format, sync message type `0x42`). Interop between the backend and
`@automerge/automerge` 3.5.0 is covered by a test in CI: loading the server document,
syncing in both directions, and non-ASCII text.

Text indices differ between libraries: automerge-go counts Unicode code points, JS counts
UTF-16 code units. Indices never go over the wire. Positions that are shared (cursors)
are Automerge cursors, see [Presence](#6-presence).

## 2. Users, roles and actors

### Users and roles

Users are configured in the backend configuration (`server.auth.users`). Each one has a
username, a bcrypt password hash, an optional display name and email, and a role:

| Role | REST | Document websocket |
|---|---|---|
| `reader` | read-only endpoints (`GET`) | receives the document and presence, sends presence. The server never applies its changes (see [Readers](#readers)). |
| `editor` | everything except user administration | full editing |
| `admin` | everything | full editing |

The old single-user configuration (`server.basicAuth.user/password`) keeps working; that
user is an `admin`.

The server assigns every user a stable colour (derived from the username, overridable in
the configuration). Clients use it for cursors, selections and authorship.

Future work: per-document or per-section permissions like CodiMD (owner, "editable by
signed-in users", "locked", "private"). The role check is one function in the backend,
so permissions can be added there without changing the protocol.

### Actor ids

Every Automerge change carries the actor id of the document that made it. The protocol
uses actor ids to attribute changes to users, so they follow these rules:

- **One actor id per document copy.** Automerge requires that two copies of a
  document which can change concurrently never share an actor id, because their
  sequence numbers would collide and break sync. Two browser tabs, or two devices,
  must use different actor ids. So:
  - When a client loads a fresh copy from `documentState`, it generates a new actor id
    (16 random bytes, lower-case hex, like `Automerge.getActorId`) and sets it on the
    copy (`Automerge.load(bytes, { actor })` in JS, `Document.load(bytes).fork(actor)`
    in automerge-java).
  - When a client persists its copy for offline use, it persists the actor id with
    it and keeps using that pair. One copy, one actor.
- The client sends the actor id of its copy in the `hello` message. On first use the
  server **binds** the actor id to the authenticated user and stores the binding
  durably (`actors.json` in the state directory, see [Storage](#9-storage)). The
  binding survives restarts and epochs and is never removed. A user can have any number
  of actor ids; authorship maps all of them to the user.
- An actor id bound to another user is refused (close code `4003`).
- Every change in an incoming sync message is checked before it is applied. Its actor
  must be bound to the authenticated user, or be unbound (it is then bound to this
  user). If any change in the message belongs to another user's actor, the whole message
  is rejected (an `error` message with code `foreign-actor`, then the socket is closed
  with `4003`).
- The server makes its own changes with a fresh random actor id for each change. It
  binds these ids to two pseudo users, which never clash with real usernames because
  real usernames cannot contain `:`:
  - `:disk` (display name "External change"): external changes of the file (git pull,
    manual edits), see [External changes](#8-external-changes)
  - `:server` (display name "Server"): the initial import of a file and restores of
    old revisions

The actor map is available over REST (`GET /actors/`) and in the websocket. The
`actors` field of `initial-content` has the whole map. An `actor` message follows when a
connection joins (its actor) and when a new actor id appears in a document (a new client
actor, or a `:disk` change). Entry:

```json
{ "actorId": "5f3c…", "username": "markus", "displayName": "Markus", "color": "#e8590c" }
```

## 3. Authentication

All REST endpoints except `GET /alive/` require authentication.

- **REST:** HTTP Basic auth (`Authorization: Basic …`) against the configured users. A
  browser sets the header itself with `fetch`. Requests that send
  `X-Requested-With: XMLHttpRequest` get no `WWW-Authenticate` header on a 401, so the
  browser does not show its own login dialog.
- `GET /auth/whoami/` returns the authenticated user:
  `{"username","displayName","role","color"}`. The web client uses it to check a login.
- **Websocket:** browsers cannot set headers on a websocket, and tokens in URLs end up in
  proxy logs. So the websocket accepts either of these:
  1. an `Authorization: Basic …` header on the upgrade request (Android/OkHttp), or
  2. a **ticket** in the `hello` message. A ticket comes from `POST /auth/ws-ticket/`
     (Basic auth) as `{"ticket":"…","expiresAt":"<RFC 3339>"}`. It is valid for 60
     seconds, can be used only once, and is not tied to a document. Tickets live in
     memory only, so a backend restart invalidates them.

  Tickets are never accepted in the URL. A connection that is not authenticated within
  10 seconds of the upgrade is closed with `4001`.
- **CORS:** `server.cors.allowedOrigins` lists the origins of web clients. The backend
  allows the `Authorization`, `Content-Type` and `X-Requested-With` headers. Preflight
  requests need no authentication. When origins are configured, the websocket upgrade
  also checks the `Origin` header against them. A request without an `Origin` header,
  like Android's, is allowed.

## 4. Document model

- One Automerge document per markdown file.
- The root map has exactly one key, `content`: a Text object (`ObjType.Text`) holding
  the full file content (UTF-8, line endings kept as they are on disk).
- The server creates the document when the file is first opened (actor **server**). The
  object is created once and never replaced. Clients must never create or reassign
  `content`; they only splice into it (`Automerge.splice` / `updateText`).
- Each server document lineage has an **epoch**, a random id created together with the
  document. It changes only when the server starts a new lineage because its saved state
  is gone. A client must not sync a document of one epoch with a
  document of another epoch, because the two have unrelated histories and merging them
  would duplicate the text.

## 5. Document websocket

`GET /document/{documentId}/ws/` upgrades to a websocket. All frames are **text frames
holding one JSON object**. Binary Automerge data is standard base64 (with padding)
inside the JSON. Every message has a `type`. Unknown types must be ignored, so new
message types can be added without breaking old clients.

### Connection flow

```
client                                        server
  | -- upgrade (optional Basic header) --------> |
  | -- hello {actorId, ticket?, epoch?} -------> |  authenticate, bind actor
  | <------------- initial-content {...} ------- |  document, epoch, actors, you
  | <------------- presence-snapshot {...} ----- |  other connections
  | <=========== sync-request (both ways) =====> |  standard Automerge sync loop
  | -- presence {selections} ------------------> |  broadcast to the others
  | <-> heartbeat every 5 s (both ways)          |
```

### Client to server

**`hello`**: must be the first message.

```json
{
  "type": "hello",
  "documentId": "5772684240079201105",
  "actorId": "5f3c9a…(32 hex chars)",
  "ticket": "…",
  "epoch": "…",
  "client": "MkDocsEditor-Web/2.0.0"
}
```

`ticket` is required unless the upgrade had a valid Basic header. `epoch` is the epoch of
a document the client already holds for this `documentId` (reconnect), otherwise it is
left out. `client` is free text for logs.

**`sync-request`**: one Automerge sync message.

```json
{ "type": "sync-request", "documentId": "…", "syncMessage": "<base64>" }
```

**`presence`**: the client's selections, see [Presence](#6-presence).

**`heartbeat`**: `{"type":"heartbeat"}`.

### Server to client

**`initial-content`**: the reply to `hello`.

```json
{
  "type": "initial-content",
  "documentId": "…",
  "epoch": "b1c2…",
  "documentState": "<base64 of Automerge save()>",
  "content": "# Plain text of the document\n",
  "readOnly": false,
  "you": { "clientId": "c-17", "username": "markus", "displayName": "Markus", "role": "admin", "color": "#e8590c" },
  "actors": [ { "actorId": "…", "username": "…", "displayName": "…", "color": "…" } ]
}
```

- If the client sent no `epoch`, or a different one: it loads `documentState`
  (`Automerge.load(bytes, { actor: myActorId })`), then starts syncing with a fresh
  sync state. If it held unsynced local edits from another epoch, it applies them as a
  text diff (see [Epochs](#epochs-and-offline-edits)).
- If the client sent the same `epoch`: it keeps its own document and only syncs.
  `documentState` is still sent and may be ignored.
- `content` is a convenience for showing text before Automerge has loaded. It is equal
  to the text of `documentState`.
- `readOnly` is true for readers. The client should make its editor read-only.

**`sync-request`**: same format as above.

**`presence`**: another connection's presence, stamped by the server.

```json
{
  "type": "presence",
  "documentId": "…",
  "clientId": "c-18",
  "username": "anna",
  "displayName": "Anna",
  "color": "#1971c2",
  "actorId": "…",
  "selections": [ { "anchor": "<cursor>", "head": "<cursor>" } ]
}
```

**`presence-snapshot`**: sent once after `initial-content`. It holds the current presence
of every other connection on this document:
`{"type":"presence-snapshot","documentId":"…","peers":[<presence>, …]}`. A peer that
connected but has not sent presence yet appears with `"selections": []`.

**`presence-leave`**: `{"type":"presence-leave","documentId":"…","clientId":"c-18"}`. Sent
when a connection closes or times out.

**`actor`**: a new actor binding (the same fields as the entries in `actors`). It is
broadcast to all connections of the document when a new actor id is bound.

**`heartbeat`**: `{"type":"heartbeat"}`.

**`document-renamed`**: `{"type":"document-renamed","documentId":"<old>","newDocumentId":"<new>"}`.
The document (or a section above it) was renamed or moved, so its id changed. The server
moves the Automerge state to the new id, sends this message and closes the socket with
`4009`. The client reconnects to the new id with the same `epoch` and keeps its
document.

**`document-deleted`**: `{"type":"document-deleted","documentId":"…"}`. The file was
deleted on disk or over REST. The socket is closed with `4004`. The client must keep its
local document and any unsynced edits, and offer to save them, for example as a new
document or by copying the text. A REST `DELETE` of a document that has connected
clients returns `409 Conflict` unless the request has `?force=true`. The same applies to
a section that contains such a document.

**`error`**: `{"type":"error","code":"…","message":"…"}`. Codes: `bad-request`,
`unauthorized`, `foreign-actor`, `read-only`, `internal`. A fatal error is followed by a
close frame.

### Sync

Both sides run the standard Automerge sync loop, with **one sync state per connection on
each side**. Sync states are never persisted.

- After receiving a message, call `receiveSyncMessage`. Then call `generateSyncMessage`
  in a loop and send every message it returns until it returns `null`.
- After a local change, call `generateSyncMessage` and send the result in the same way.
- The server applies the changes from a client to the authoritative document. Then it
  generates and sends sync messages for **every** connection of the document (this is
  how edits reach the other users) and schedules the disk write.
- Clients should batch keystrokes, for example by generating at most one sync message
  every 50 to 100 ms while the user types.

### Readers

A reader's connection syncs against a private fork of the document (a *shadow*). The
server merges every change of the main document into the shadow. Changes from the reader
are applied to the shadow only, never to the main document, and the reader gets an
`error` with code `read-only`. Readers stay in sync this way, and a misbehaving reader
cannot change the document. The shadow is thrown away on disconnect.

### Heartbeat and timeouts

Each side sends `heartbeat` every 5 seconds if it has sent nothing else in that time.
The server closes a connection it has heard nothing from for 15 seconds (`4008`) and
broadcasts `presence-leave`. Clients should do the same in the other direction:
reconnect after 15 seconds without a message from the server.

### Close codes

| Code | Meaning | Client action |
|---|---|---|
| 1000 | normal | none |
| 1001 | server shutting down | reconnect with backoff |
| 4001 | not authenticated (missing or invalid credentials or ticket, `hello` timeout) | log in again, get a new ticket |
| 4003 | forbidden (actor id of another user, foreign changes) | generate a new actor id only if the server refused the binding; report the bug |
| 4004 | document not found or deleted | close the editor |
| 4008 | heartbeat timeout | reconnect |
| 4009 | document renamed | reconnect to `newDocumentId` |
| 4400 | protocol error (invalid JSON, first message not `hello`, ...) | fix the client |

### Epochs and offline edits

A client may keep editing while disconnected. It persists its document, the `epoch`, and
the **server heads**: the heads the server is known to have, which are the shared heads
of the last completed sync (`Automerge.getHeads` after a sync round with nothing left to
send). On reconnect it sends its `epoch` in `hello`:

- **Same epoch:** keep the document and sync. Automerge merges the offline changes.
- **Different epoch:** the server lost its saved state and started a new lineage from
  the file. The client must not sync its old document. Instead it:
  1. loads `documentState` as its new document,
  2. computes the text diff from its old document at the server heads
     (`Automerge.view(old, serverHeads)`) to the current text of its old document. This
     diff is exactly its unsynced edits.
  3. applies that diff as splices to the new document and syncs.

  This is the same three-way merge the server uses for external changes, so the
  client's edits survive. The client keeps the old document until this sync has
  completed.

## 6. Presence

Presence is ephemeral. The server relays it and never stores it.

- A client sends `presence` whenever its cursor or selection changes, throttled to at
  most one message every 100 ms:

  ```json
  { "type": "presence", "documentId": "…", "selections": [ { "anchor": "<cursor>", "head": "<cursor>" } ] }
  ```

  `selections` is a list, so multiple selections are possible. The first entry is the
  primary one. A plain cursor is a selection with `anchor == head`. An empty list means
  "no cursor in this document" (editor not focused).
- `anchor` and `head` are **Automerge cursors** on the `content` text, as strings:
  `Automerge.getCursor(doc, ["content"], index)` in JS, `Document.makeCursor` in
  automerge-java (`Cursor.toString()` / `Cursor.fromString`). The receiver resolves them
  with `getCursorPosition` (JS) or `lookupCursorIndex` (Java). The string is the
  Automerge core cursor encoding. The server does not parse it. JS and Java must agree
  on it, and the clients' interop tests check this. Cursors stay
  correct across concurrent edits, unlike indices, and do not depend on UTF-16 versus
  code point indexing. A receiver that cannot resolve a cursor yet, because it has not
  received the change it points into, keeps the last position and retries after the
  next sync.
- The server overwrites `clientId`, `username`, `displayName`, `color` and `actorId`
  with the values of the authenticated connection, so presence cannot be forged.
- The server sends a `presence-snapshot` on join and `presence-leave` on disconnect or
  timeout.
- One user can have several connections (two devices, two tabs). The list of online
  users is the set of distinct usernames in the presence list.

Future work: an events websocket (`/events/ws/`) that pushes tree changes
(create/rename/delete) and "who is in which document" for the file browser.

## 7. Authorship

The server computes per-character authorship, so clients do not need a history API
(automerge-java has none, see below):

```
GET /document/{id}/authorship/
{
  "heads": ["<hex change hash>", …],
  "runs": [ { "start": 0, "end": 12, "actorId": "…", "username": "markus" }, … ]
}
```

- `runs` covers the text at `heads` without gaps. `start` is inclusive and `end`
  exclusive. Both are positions in **Unicode code points**; JS clients convert them to
  UTF-16 indices.
- The server replays the document history in causal order and attributes every
  inserted text range to the change's actor (a text diff per change). `username` comes
  from the actor map. `:disk` and `:server` runs are external or imported text.
- Clients fetch it when they open a document and then update it themselves: text a
  local or remote change inserts belongs to that change's actor (from the sync
  messages, or from `Automerge.diff` in JS). Fetch it again after a reconnect, or when
  the local bookkeeping gets out of step.

For a coarser view of the committed history, the
[revision history](#11-revision-history) endpoints return the git authors per commit.

## 8. External changes

The file on disk can change outside the backend (git pull, manual edits, other tools).
For every loaded document the server stores the **disk base**: the text it last read
from or wrote to disk, and the Automerge heads that text corresponds to.

When the file watcher reports a change, or a document is loaded from saved state, the
server compares the file with the disk base. If they differ:

1. Fork the document at the disk-base heads.
2. Compute the diff from the disk-base text to the new file text (diff-match-patch,
   code point positions) and apply it to the fork as splices, with the **disk** actor.
3. Merge the fork into the document and sync all connections.
4. The new disk base is the file text with the heads of the disk change.

Edits that users made since the disk base are concurrent with the disk change, and
Automerge merges them like edits from two users. Nothing is lost. If the file was
deleted, connected clients get `document-deleted`. A file whose content is not valid
UTF-8 is not opened for editing.

The debounced disk write (below) writes the merged text back, so the file ends up with
both the external and the concurrent edits.

## 9. Storage

### State directory

The Automerge state lives outside the docs folder, in `mkdocs.stateDir` (default
`<projectPath>/.mkdocseditor/`):

```
.mkdocseditor/
  actors.json                 actor id -> username bindings, never shrinks
  documents/
    <uuid>.automerge          Automerge save() of the document
    <uuid>.json               {"path": "guide/setup.md", "epoch": "…", "diskBaseHeads": [...], "diskBaseHash": "…"}
```

- Documents have a stable internal uuid. The REST `documentId` is derived from the path
  (it always was), so the sidecar `path` maps one to the other. A rename only rewrites
  the sidecar. The server builds the path index from the sidecars at startup.
- On startup the backend adds the state directory to `.git/info/exclude` of the wiki
  repository, if there is one, so it is never committed. MkDocs only reads `docs_dir`,
  so the state is never published. The backend refuses to start if the state directory
  is inside the docs folder.
- Loaded documents are saved with a full `save()` at most every 2 seconds while they
  change, and when the last client leaves. Documents without connections are unloaded
  from memory.
- The server never deletes a state file. If one cannot be loaded, it is renamed to
  `<uuid>.automerge.corrupt-<timestamp>` and kept. The server then creates a new
  document from the file (new epoch). Clients holding the old epoch re-apply their
  edits as described in [Epochs](#epochs-and-offline-edits).
- A deleted document's state is moved to `documents/deleted/`. If a file with the
  same path appears again (a restore from git, an undo), the server restores that
  document, epoch included, and merges the file content as an external change. Offline
  clients of the document can then still sync.

### Disk writes

The server writes the document text to the markdown file at most every 2 seconds while
it changes, and immediately when the last client leaves. The write is atomic (temporary
file plus rename), so MkDocs and the wiki stay current. The server remembers the text it
wrote, so the file watcher does not treat its own write as an external change.

A write never overwrites text that is not in the document yet. Under the document's
lock, the server first reads the file and compares it with the disk base. If the file
changed (an external edit the watcher has not reported yet), the server merges it first
([External changes](#8-external-changes)) and writes the merged text. A git commit only
stages files after such a write, so it never contains less than the merged document.

### History size

The Automerge history only grows, and the server never truncates it. Wiki pages are
small: a page with years of edits is a few hundred kilobytes. Automerge stores the full
history compactly, and `save()` compresses it.

If compaction is ever needed, it must not lose offline edits. The server would keep
every old-epoch document, so it can load changes made against it. When such a change
arrives, the server merges it into the old document and applies the resulting text diff
to the current document. Until that exists, there is no compaction.

## 10. Git integration

Optional, `git.enabled: false` by default. The backend commits only files it wrote
itself (document edits, created, renamed, deleted and uploaded files inside the docs
folder). It never stages anything else, so unrelated uncommitted changes in the
repository stay untouched.

### When it commits

Commits are batched per document:

| Trigger | Default | Setting |
|---|---|---|
| all users have left the document for N minutes | 5 | `git.commitAfterLeave` |
| no change for M minutes, even while clients are connected | 10 | `git.commitAfterIdle` |
| at most every K minutes during continuous editing | 60 | `git.commitMaxInterval` |
| server shutdown (SIGTERM) | always | |

Tree operations over REST (create, rename, delete, upload) are committed right away,
together with pending edits of the affected files.

### Authors

- The author of an edit commit is the user with the most Automerge changes in that file
  since the last commit, from the actor map. Name and email come from the user
  configuration (`displayName`, `email`). Without an email the backend uses
  `<username>@mkdocseditor.invalid`.
- The other contributors are added as `Co-authored-by: Name <email>` trailers.
- Changes by the **disk** actor do not count. A commit that holds only disk changes is
  not made, because those changes came from git or from someone outside the backend.
- A REST tree operation is authored by the user who made the request.
- The committer is the backend (`git.committerName`, `git.committerEmail`).

Commit message: `Update guide/setup.md` (or `Create`, `Rename … to …`, `Delete`,
`Upload`), followed by the trailers.

### Push

`git.push: false` by default. When enabled, the backend runs `git push` after each
commit to the configured remote. Credentials come only from the environment or
mounted files (an SSH key with `GIT_SSH_COMMAND`, or a credential helper), never from
the repository or the backend configuration. A failed push is logged and retried after
the next commit.

## 11. Revision history

Based on the git history of each file. Requires `git.enabled`.

| Method | Path | Description |
|---|---|---|
| `GET` | `/document/{id}/history/` | Commits that touched the file, newest first: `[{"revision","time","author":{"name","email","username"},"coAuthors":[…],"message"}]` |
| `GET` | `/document/{id}/history/{revision}/content/` | The file content at that revision (`text/plain`) |
| `GET` | `/document/{id}/history/diff/?from={rev}&to={rev}` | Unified diff between two revisions. `to` may be `current` for the live text. |
| `POST` | `/document/{id}/history/{revision}/restore/` | Editors and admins only. The server sets the live document to the content of that revision. |

A restore is applied as splices (the diff from the current text to the old one) by the
**server** actor, so connected clients simply see an edit, and it can be undone like any
other edit. The restore is committed with the requesting user as author:
`Restore guide/setup.md to <short revision>`.

History follows renames (`git log --follow`).

## 12. Changes from the previous protocol

The Automerge protocol before this version was never usable. It created a new document
for every message and read every frame twice. Version 1 keeps the message names
`initial-content` and `sync-request` and adds everything else. Clients must:

- send `hello` with a persisted actor id as the first message,
- load `documentState` instead of building their own document, and never write `content`
  as a whole,
- run a real sync loop (`generateSyncMessage`/`receiveSyncMessage`) instead of applying
  sync messages as changes,
- handle the presence, heartbeat, rename and delete messages and the close codes.
