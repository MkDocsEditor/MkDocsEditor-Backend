# Dashboard: pins and the change feed

The clients' dashboard shows pinned documents and what changed in the wiki. The backend
keeps both in `activity.db`, a SQLite database in the state directory, next to `auth.db`.
The feed does not read git: it works without `git.enabled`, and it knows things git does
not, such as who edited together and which headings an edit touched.

`GET /server-info/` lists `pins` and `feed` in `features` on a server with these endpoints.
The REST details are in the [OpenAPI file](../openapi-description.yaml).

## Pins

There are two lists:

- **Family pins** are the same for everyone. Editors and admins change them.
- **Personal pins** belong to one account. Every account has them, readers too.

Only documents can be pinned.

| Request | Who |
|---|---|
| `GET /pins/` | everyone |
| `PUT` / `DELETE /pins/family/{documentId}/` | editors and admins; readers get 403 `forbidden` |
| `PUT` / `DELETE /pins/personal/{documentId}/` | every account |

App tokens with the scope `read` get 403 for every `PUT` and `DELETE`. The changes are
idempotent: pinning a pinned document or unpinning a document that is not pinned changes
nothing. All of them answer the body of `GET /pins/`. An unknown document id answers 404
`not-found`.

```json
{
  "family": [
    {
      "documentId": "17683270065320168437",
      "path": "guide/setup.md",
      "name": "setup",
      "pinnedBy": { "accountId": "…", "username": "bob", "displayName": "Bob", "color": "#2f9e44", "avatar": null },
      "pinnedAt": "2026-11-06T21:15:25.182Z"
    }
  ],
  "personal": []
}
```

Both lists are newest first. `pinnedBy` is the account as it is now, see [Authors](#authors).

The backend stores pins by path, so:

- Pins follow moves and renames through the API, also when a section that contains the
  document moves.
- Deleting a document or a section through the API removes its pins.
- A pinned document that is missing on disk, for example after a change outside the API,
  is left out of the lists, but its pin stays. When the file comes back, the pin
  shows again.

After a change the events websocket sends `{"type": "pins-changed"}`. A change of the
family pins goes to every connection, a change of personal pins only to the connections of
that account. That includes changes caused by moves and deletes.

## Feed

`GET /feed/?before=<cursor>&limit=<1..200>` returns one page of entries, newest first.
`limit` defaults to 50. `next` is the cursor of the next page, `null` on the last page.
Cursors are opaque; an invalid cursor or limit answers 400.

```json
{
  "entries": [
    {
      "id": "42",
      "time": "2026-11-06T21:15:25.182Z",
      "kind": "edited",
      "itemType": "document",
      "documentId": "17683270065320168437",
      "path": "guide/setup.md",
      "authors": [ { "accountId": "…", "username": "alice", "displayName": "Alice", "color": "#e8590c", "avatar": null } ],
      "summary": { "linesAdded": 5, "linesRemoved": 1, "headings": [ "Setup", "Install" ] }
    },
    {
      "id": "41",
      "time": "2026-11-06T21:02:10.000Z",
      "kind": "moved",
      "itemType": "section",
      "path": "manual",
      "oldPath": "guide",
      "authors": [ { "accountId": "…", "username": "bob", "displayName": "Bob", "color": "#2f9e44", "avatar": null } ]
    }
  ],
  "next": "MTc2MjQ2MzMyNTE4Mi40Mg"
}
```

| Field | Meaning |
|---|---|
| `kind` | `edited`, `created`, `moved` or `deleted` |
| `itemType` | `document` or `section` |
| `documentId` | the current id of the document, only if a document exists at `path` |
| `path` | path relative to the docs folder; for moves the new path |
| `oldPath` | only for `moved` |
| `authors` | who made the entry, in the order of their first change |
| `summary` | only for `edited` |

Entries are kept for 365 days.

### Edits

The backend records an edit when it writes a document file. It writes at most every 2
seconds while a document changes, see [sync-protocol.md](sync-protocol.md#disk-writes).
To keep the feed readable, edits form **editing sessions**:

- An edit within 15 minutes of the last edit of the same document extends that entry:
  the authors are merged, the summary compares the text before the session with the
  current text, and `time` moves to the latest edit. So an entry can move up the feed.
- If the document is back at the text the session started with, the entry is removed.
- A move, a delete or a create of the document or a section around it ends the session.

`summary` counts the changed lines (a changed line counts as one removed and one added)
and names up to 5 headings above the changed lines, in document order. Headings are
Markdown ATX headings (`#` to `######`) outside fenced code blocks.

Edits of the server alone are no entries: the reference updates of a move are part of the
`moved` entry. A restore from the history is an edit of the account that restored.

Changes of a file on disk while the document is open in an editor are edits of
`External change`. Changes of files that nobody has open are not in the feed.

### Created, moved, deleted

Creates, moves, renames and deletes of documents and sections through the REST API are
entries with the requesting account as author. A rename is a `moved` entry. Moving a
section is one entry for the section, not one per document inside. Resources are not in
the feed.

After the feed changed, the events websocket sends `{"type": "feed-changed"}`, at most
once per 10 seconds.

## Authors

Authors and `pinnedBy` show the account as it is now: a changed display name, color or
avatar applies to old entries too.

```json
{ "accountId": "…", "username": "alice", "displayName": "Alice", "color": "#e8590c", "avatar": "3f2a…" }
```

`avatar` is the version of the profile picture at `/avatars/{accountId}/{avatar}/`, see
[auth.md](auth.md). The server itself has no `accountId` and no `username`:

| `displayName` | Meaning |
|---|---|
| `External change` | a change of the file on disk |
| `Server` | the backend itself |

Both have the color `#868e96` and the avatar `null`.
