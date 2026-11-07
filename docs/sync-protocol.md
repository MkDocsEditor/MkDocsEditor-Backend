# MkDocsEditor sync protocol and architecture

This document is the contract between the backend and the clients (web, Android) for
realtime collaborative editing. It covers authentication, the document websocket
(Automerge sync, presence, authorship), revision history, and how the backend stores
documents and commits them to git.

Status: **version 1**. Implemented: authentication, roles, the document websocket,
presence, authorship, persistence, external changes, readers, rename and delete, git
integration, revision history and the events websocket. The REST API reference is
[`openapi-description.yaml`](../openapi-description.yaml). Where the two disagree on a
REST endpoint, the OpenAPI file wins. For the websocket protocol, this document wins.

**Paths.** The backend serves every route under the fixed prefix `/api`: `/api/server-info/`,
`/api/auth/…`, `/api/document/{id}/ws/`, `/api/events/ws/`. Other paths answer 404, so the
web client, the MkDocs site and the API can share one host. The paths in these documents and
in the OpenAPI file are written without the prefix. `backendUrl` (connect links, `GET
/server-info/`) includes it, for example `https://wiki.markusressel.de/api`, and clients
append the paths to it.

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

The server assigns every user a stable colour (derived from the username, overridable in
the configuration). Clients use it for cursors, selections and authorship.

Accounts can have a profile picture. Everywhere the protocol has `color` for an account, it
also has `avatar`: the version of the picture, or `null` without one (also for `:disk` and
`:server`). Clients load it from `GET /avatars/{accountId}/{avatar}/`, see
[auth.md](auth.md#avatars). Servers with avatars list `avatars` in `features` of
`GET /server-info/`.

**Profile changes.** When an account changes its display name, colour or avatar, the server
sends, in every open document, an `actor` message for each actor of the account and a
`presence` of each connection of the account (to the other connections), and a new `online`
list on the events websocket. Clients replace what they show for that account.

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
  server **binds** the actor id to the authenticated user's account id and stores the
  binding durably (in the account database `auth.db` in the state directory, see
  [Storage](#9-storage); older versions used `actors.json`, which is imported once). The
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
{ "actorId": "5f3c…", "accountId": "k3x8m2q9w4e7r1t6y5u0i3o8pa", "username": "markus", "displayName": "Markus", "color": "#e8590c", "avatar": "3f9a1c0b7d2e4a68" }
```

`accountId` is the stable id of the account (empty for `:disk` and `:server`).
`username` is the account's current username, which can change; clients that keep
authorship or actor data should key it on `accountId`. Presence messages, `you`, the
`online` list, authorship runs and history authors carry `accountId` as well.

## 3. Authentication

> Accounts, registration, sessions, passkeys and TOTP are specified in
> [`auth.md`](auth.md) (sections 4 to 6 for request authentication, CSRF and websockets).
> This section is the summary a sync client needs.

All REST endpoints except `GET /alive/`, `GET /auth/status/` and the login and link
endpoints require authentication.

- **REST:** a session, either the HttpOnly session cookie (web client) or
  `Authorization: Bearer mds_…` (native clients), or an app token.
- `GET /auth/whoami/` returns the authenticated user:
  `{"id","username","displayName","role","color","authMethod",…}`. `id` is the stable
  account id; the username can change.
- **Websocket:** browsers cannot set headers on a websocket, and tokens in URLs end up in
  proxy logs. So the websocket accepts any of these:
  1. the session cookie on the upgrade request, only when the `Origin` is the backend
     itself or listed in `server.cors.allowedOrigins`,
  2. an `Authorization: Bearer …` (or, during the transition, `Basic …`) header on the
     upgrade request (Android/OkHttp), or
  3. a **ticket** in the `hello` message. A ticket comes from `POST /auth/ws-ticket/` as
     `{"ticket":"…","expiresAt":"<RFC 3339>"}`. It is valid for 60 seconds, can be used
     only once, is not tied to a document, and is bound to the session that requested
     it: once that session ends, the ticket is invalid. Tickets live in memory only, so a
     backend restart invalidates them.

  Tickets are never accepted in the URL. A connection that is not authenticated within
  10 seconds of the upgrade is closed with `4001`. When the session of a connection ends
  (logout, revocation, password reset) or its account is disabled, the backend closes the
  connection with `4001`; the client must log in again instead of reconnecting.
- **CORS:** `server.cors.allowedOrigins` lists the origins of web clients. The backend
  allows credentials from them and the `Authorization`, `Content-Type` and
  `X-Requested-With` headers, and exposes `Retry-After`. Preflight requests need no
  authentication. When origins are configured, the websocket upgrade also checks the
  `Origin` header against them. A request without an `Origin` header, like Android's, is
  allowed, but then only with a bearer header or a ticket.

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
  "you": { "clientId": "c-17", "accountId": "k3x8…", "username": "markus", "displayName": "Markus", "role": "admin", "color": "#e8590c", "avatar": null },
  "actors": [ { "actorId": "…", "username": "…", "displayName": "…", "color": "…", "avatar": null } ]
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
  "accountId": "k3x8m2q9w4e7r1t6y5u0i3o8pa",
  "username": "anna",
  "displayName": "Anna",
  "color": "#1971c2",
  "avatar": "3f9a1c0b7d2e4a68",
  "actorId": "…",
  "instanceId": "0f8a…",
  "deviceName": "Pixel",
  "instanceColor": "#2b8a3e",
  "lastActiveAt": "2026-11-05T09:30:12.123Z",
  "selections": [ { "anchor": "<cursor>", "head": "<cursor>" } ]
}
```

**`presence-snapshot`**: sent once after `initial-content`. It holds the current presence
of every other connection on this document:
`{"type":"presence-snapshot","documentId":"…","peers":[<presence>, …],"lastPositions":[…],"ghosts":[…]}`.
A peer that connected but has not sent presence yet appears with `"selections": []`.
`lastPositions` says where accounts last were, see [Presence](#6-presence), and `ghosts` lists
instances that left recently, see [Ghosts](#ghosts).

**`presence-leave`**: `{"type":"presence-leave","documentId":"…","clientId":"c-18","instanceId":"…"}`.
Sent when a connection closes or times out. If this was the last connection of its client
instance, it can also have `accountId`, `selections`, `viewport`, `deviceName`,
`instanceColor`, `avatar` and `ghostMs`, see [Ghosts](#ghosts).

**`actor`**: a new actor binding (the same fields as the entries in `actors`). It is
broadcast to all connections of the document when a new actor id is bound, and again when
the profile of its account changes.

**`heartbeat`**: `{"type":"heartbeat"}`.

**`document-renamed`**: `{"type":"document-renamed","documentId":"<old>","newDocumentId":"<new>"}`.
The document (or a section above it) was renamed or moved, so its id changed. The server
moves the Automerge state to the new id, sends this message and closes the socket with
`4009`. The client reconnects to the new id with the same `epoch` and keeps its
document. A client that connects later with the old id (it was offline during the move)
gets this message right after `hello`; the backend keeps a durable table of old ids for
that, see [references.md](references.md#7-ids-after-a-move-redirects).

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
- Two literal strings are reserved for positions where no character exists to point at:
  `"end"` is the caret after the last character, and `"start"` is position 0 of an
  empty text. automerge-java cannot make a cursor at index `length`
  (`makeCursor(length)` throws), so clients send `"end"` there and `"start"` when the
  text is empty. Every client checks for these two strings before calling the Automerge
  cursor lookup: `"start"` resolves to 0, `"end"` to the current text length. A client
  may send them in other cases too (for example `"start"` for index 0 of a non-empty
  text), so receivers always handle both.
- The server overwrites `clientId`, `instanceId`, `deviceName`, `accountId`, `username`, `displayName`, `color`, `avatar` and `actorId`
  with the values of the authenticated connection, so presence cannot be forged.
- `lastActiveAt` (RFC 3339, in presence and in the peers of `presence-snapshot`) is stamped
  by the server: the time of the last presence message of this connection, also one that only
  changes the viewport, or the time it connected if it has not sent one. Servers that have it list `join-presence` in `features`
  of `GET /server-info/`. A join link picks the sender's connection with the newest value,
  see [auth.md](auth.md#access-request-and-join-link).
- `lastPositions` in `presence-snapshot` holds, per account, the last non-empty selections
  and the last viewport in this document, newest first, also for accounts that are offline or
  online without a cursor: `{"accountId", "username", "displayName", "color", "avatar", "selections",
  "viewport"?, "lastActiveAt"}`. `selections` is empty if the account only sent
  a viewport. `lastActiveAt` is the time of the newest selections or viewport in this entry.
  Entries are kept for 30 days, survive restarts and moves, and end with the epoch, because
  cursors are only valid in their epoch. Selections or a viewport over 4 KiB are not kept, and
  deleted accounts are left out. The list can be empty, and servers from before this field do
  not send it. A cursor can point into text that was deleted since, so the client falls back
  to the start if it cannot resolve it.
- The server sends a `presence-snapshot` on join and `presence-leave` on disconnect or
  timeout.
- One user can have several connections (two devices, two tabs). The list of online
  users is the set of distinct account ids in the presence list.

### Client instances

Every client instance behaves the same, whatever its account, device or client type. An
instance is one tab or one app installation, and it can reconnect and reload.

- `hello` can have `instanceId`, 32 lowercase hex characters that the client generates and
  keeps across reconnects and reloads. Anything else closes the connection with 4400. Without
  it, the server uses `clientId` as the instance id. The server also uses `clientId` if another
  account has this id in the document, so nobody can pose as an instance of someone else.
  `you.instanceId` in `initial-content` is the id in use.
- `presence`, and `peers` in `presence-snapshot`, have `instanceId` and `deviceName`, stamped by
  the server. `deviceName` is the device name of the login session, else `client` of `hello`,
  and is missing if there is neither.
- `presence` from a client can have `instanceColor`: `"#rrggbb"` sets it, `null` clears it, and
  a missing one leaves it unchanged. `hello` also accepts it, so a reload has the color in its
  first presence. It belongs to the connection and never changes the account color or
  authorship. `color` stays the account color. Clients draw `instanceColor`, or else `color`.
- Servers with client instances list `instance-presence` in `features` of `GET /server-info/`.

### Viewport

Presence can have an optional `viewport`, the text the user is looking at:

```json
{ "type": "presence", "documentId": "…", "selections": [], "viewport": { "top": "<cursor>", "bottom": "<cursor>" } }
```

- `top` and `bottom` are cursors of the first and last visible character, in the format of
  selections, including `"start"` and `"end"`. They stay correct across edits and do not
  depend on the screen size or on line wrapping.
- Clients always send their current `selections`. Empty selections with a viewport mean "no
  cursor", and the server keeps the last selections in `lastPositions`.
- From a client, a missing `viewport` keeps the last one, and `"viewport": null` clears it.
  Clients send null only when they show nothing of the document, so followers do not track a
  stale spot. From the server, presence is always complete: a missing `viewport` means there
  is none.
- A client that shows the rendered preview, alone or next to the editor, sends the source
  range of the visible blocks:
  - A block covers its source lines up to the line before the first line of the next block.
    The last block covers the rest of the text.
  - The visible blocks are those on screen. A block taller than the view counts when it
    covers the top of the view.
  - `top` is the start of the first line of the first visible block, and `bottom` the end of
    the last line of the last visible block.
- Clients send it at most about twice per second while scrolling, and once more when
  scrolling stops.
- A follower centers the middle of `top` and `bottom`, on any screen size. Without a viewport
  it scrolls to the cursor.

### Ghosts

When a client instance leaves, its cursor stays as a ghost for 60 seconds, so following survives
a network problem or a quick switch to another app. This also holds for other instances of the
same account and the same login session.

```json
{ "type": "presence-leave", "documentId": "…", "clientId": "c-18", "instanceId": "…", "accountId": "…", "selections": [ … ], "viewport": { … }, "deviceName": "Pixel", "instanceColor": "#2b8a3e", "avatar": "3f9a1c0b7d2e4a68", "ghostMs": 60000 }
```

- `accountId`, `selections` and `ghostMs` are only present when the last connection of the
  instance left, and that connection sent a cursor or a viewport. Otherwise there is no ghost,
  for example if another connection of the instance is still there because of a half-open socket.
  `viewport`, `deviceName`, `instanceColor` and `avatar` are present if there are any.
- The position is the last non-empty cursor and viewport of that connection. A client that clears
  its position when it goes to the background keeps its ghost where it was.
- `ghostMs` is a duration and counts from receipt, so differing clocks do not matter. A
  client that joins during that time finds the ghost in `ghosts` of `presence-snapshot`:
  `{"instanceId", "accountId", "username", "displayName", "color", "avatar", "instanceColor"?,
  "deviceName"?, "selections", "viewport"?, "ghostMs"}`, the most recent first. `ghostMs` is the
  remaining time. The list can be empty, ghosts survive unloading the document, and
  deleted accounts are left out. The ghost of the joining instance is not in it.
- A `presence` of the same `instanceId` ends the ghost. A join of that instance ends it on the
  server and sends such a presence, so a reload shows nothing once it is back.
- **Following:** clients follow an instance. Following continues while its ghost lasts, resumes
  when it returns, and ends when the ghost expires.
- **Join links:** an instance that is a ghost still counts as present, so the link turns on
  following. See [auth.md](auth.md#access-request-and-join-link).

### Events websocket

`GET /events/ws/` pushes changes for the file browser. It authenticates like the
document websocket: an `Authorization` header on the upgrade request, or a ticket in the
first message. The first message is always `hello`:

```json
{ "type": "hello", "ticket": "<optional ticket>", "client": "web 1.0" }
```

Every role may connect. The server then sends:

- `{"type": "online", "documents": [{"documentId": "…", "users": [{"accountId", "username", "displayName", "color", "avatar"}]}]}`:
  the users connected to each document. Each user is listed once per document, even
  with several connections, and documents without users are left out. The server
  sends this list right after `hello` and again whenever someone opens or leaves a
  document or changes their profile (collected for 100 ms). Each message is the complete list, so the client
  replaces its copy.
- `{"type": "tree-changed"}`: the tree changed (create, rename, delete, upload, or a
  change on disk). The client fetches the tree (`GET /section/`) again. Events can be lost
  while a client is disconnected, so the client also fetches the tree after every
  (re)connect.
- `{"type": "items-moved", "moves": [{"type", "oldId", "newId", "oldPath", "newPath"}]}`:
  items were moved or renamed, sent before `tree-changed`. Clients re-key open editors,
  tabs and caches; see [references.md](references.md#7-ids-after-a-move-redirects).
- `{"type": "access-requests", "pending": 2}`: only for admins who connected with a
  session, never for other roles or app tokens. It holds the number of pending access
  requests, and comes right after the first `online` and whenever the number changes, also when a
  request expires (checked once per minute). See
  [auth.md](auth.md#access-request-and-join-link).
- `{"type": "pins-changed"}`: pins changed, see [dashboard.md](dashboard.md#pins).
  Changes of the family pins go to everyone, changes of personal pins only to the
  connections of that account. The client fetches `GET /pins/` again.
- `{"type": "feed-changed"}`: the feed has new or changed entries, at most once per
  10 seconds. The client fetches the first page of `GET /feed/` again, see
  [dashboard.md](dashboard.md#feed).
- `{"type": "heartbeat"}`, as on the document websocket.

The client sends `{"type": "heartbeat"}` at least every 5 seconds. Without any
message for 15 seconds the server closes with 4008. Other client messages are
ignored. Close codes are those of the document websocket (4001 without valid
authentication, 4400 for a bad first message, 1001 on shutdown).

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
- A replay takes about 0.5 ms per change for a page of 20 KB, and history is never
  compacted, so the server avoids repeating it. It stores the replay state as a snapshot
  next to the document and only applies the changes made since then. The snapshot is
  updated in the background every 200 changes and when a changed document is unloaded,
  also if nobody asked for its authorship.
- Up to 100 new changes are applied within the request. More wait for a background job,
  which concurrent requests share. The first request of a document without a snapshot
  replays the whole history once.
- A snapshot that cannot be used is ignored and the history is replayed: a different
  format version or epoch, heads the document does not have, runs that do not add up to
  the text at those heads, or an unreadable file. Increase `authorshipSnapshotVersion`
  whenever a change of the replay or the diff gives other runs for the same history.
- Readers may call it. Adjacent runs of the same actor are merged.
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
  auth.db                     accounts and actor id -> account bindings (SQLite, see auth.md)
  activity.db                 pins, the change feed, push subscriptions, QR codes (SQLite, see dashboard.md)
  push-vapid.key              VAPID key of the push messages (see push.md)
  documents/
    <uuid>.automerge          Automerge save() of the document
    <uuid>.json               {"path": "guide/setup.md", "epoch": "…", "lastPositions": {...}, "diskBaseHeads": [...], "diskBaseHash": "…"}
    <uuid>.authorship         optional authorship snapshot, moves with the document's other state files
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

The triggers are checked every 15 seconds. Before committing a document the backend
writes its current text to disk. The uncommitted change counts per document and user
are kept in `<stateDir>/git-pending.json`, so a crash or a restart does not lose the
attribution: the edits are committed by the triggers after the restart. The commit
includes only the paths of that document or tree operation (`git commit --only`), so
other staged or unstaged work in the repository is left as it is. The backend runs git
with `safe.directory=*`, because the repository may belong to another uid (an NFS
volume), and without commit signing or hooks.

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

Commit message: `Update guide/setup.md` (or `Create`, `Rename … to …`, `Move … to …`,
`Delete`, `Upload`), followed by the trailers. A move or rename lists the documents whose
references it updated in the body, see [references.md](references.md#10-git-audit-log-and-permissions).

### Push

`git.push: false` by default. When enabled, the backend runs `git push` after each
commit, to `git.remote` and `HEAD:git.branch` when they are set, otherwise to git's
default upstream. Credentials come only from the environment or
mounted files (an SSH key with `GIT_SSH_COMMAND`, or a credential helper), never from
the repository or the backend configuration. A failed push is logged and retried after
the next commit.

## 11. Revision history

Based on the git history of each file. Requires `git.enabled`.

| Method | Path | Description |
|---|---|---|
| `GET` | `/document/{id}/history/?limit=100` | Commits that touched the file, newest first: `[{"revision","time","author":{"name","email","username"},"coAuthors":[…],"message","path"}]`. `path` is the file's path in that commit. `username` (current username) and `accountId` are empty for authors who are not accounts. |
| `GET` | `/document/{id}/history/{revision}/content/` | The file content at that revision (`text/plain`) |
| `GET` | `/document/{id}/history/diff/?from={rev}&to={rev}` | Unified diff between two revisions (`text/plain`, empty if equal). `to` defaults to `current`, the live text; `from` may be `current` too. |
| `POST` | `/document/{id}/history/{revision}/restore/` | Editors and admins only. The server sets the live document to the content of that revision. |

A restore is applied as splices (the diff from the current text to the old one) by the
**server** actor, so connected clients simply see an edit, and it can be undone like any
other edit. The restore is committed right away with the requesting user as author:
`Restore guide/setup.md to <short revision>`. Pending edits of the document go into
the same commit, their authors as `Co-authored-by:` trailers.

A revision is a full or abbreviated commit hash (7 to 64 lowercase hex characters) that
appears in the history of the document. Anything else is 404. Without `git.enabled`
all history endpoints return 404.

History follows renames, but not copies: a new page starts empty and would otherwise
look like a copy of any other empty file. The backend follows a file back to the
commit that added it and continues with the old path only if that commit renamed it.

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
