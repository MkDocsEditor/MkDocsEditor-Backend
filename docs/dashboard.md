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

Both lists are in their order: a new pin goes to the end, and the order can be changed, see
[Order](#order). `pinnedBy` is the account as it is now, see [Authors](#authors).

The backend stores pins by path, so:

- Pins follow moves and renames through the API, also when a section that contains the
  document moves.
- Deleting a document or a section through the API removes its pins.
- A pinned document that is missing on disk, for example after a change outside the API,
  is left out of the lists, but its pin stays. When the file comes back, the pin
  shows again.

### Order

`PUT /pins/family/order/` and `PUT /pins/personal/order/` set the order of a list, with the
same rights as pinning in it:

```json
{ "documentIds": ["17683270065320168437", "4120567934781623001"] }
```

The listed documents come first, in this order. Ids that are not pinned in that list are
ignored, and the pins that are not listed follow in their order. The answer is the body of
`GET /pins/`. `features` of `GET /server-info/` contains `pin-order` on a server with these
endpoints. Pins keep their place when their document moves.

Pins from before the order existed keep the order they were listed in, newest first.

### Events

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
Markdown ATX headings (`#` to `######`) outside fenced code blocks and outside the front
matter. Setext headings (a line underlined with `===` or `---`) are not recognized.

### Changed lines

`features` of `GET /server-info/` contains `feed-ranges` on a server where `summary` also
says which lines changed, so clients can highlight them:

```json
"summary": {
  "linesAdded": 5, "linesRemoved": 1, "headings": [ "Setup" ],
  "textHash": "9f2c1a0b7d3e4f55",
  "ranges": [
    { "line": 12, "count": 3, "heading": "Setup", "headingOccurrence": 1, "offset": 4 },
    { "line": 30, "count": 0, "heading": "Install", "headingOccurrence": 1, "offset": 2 }
  ]
}
```

Like the counts, the ranges compare the text at the start of the editing session with the
text at its last edit. They are no union of the single edits. All line numbers refer to the
text after the last edit.

- **Lines.** The text is split on `\n` only, and a `\r` stays part of its line. A newline at
  the end of the text does not start another line, so `"a\nb\n"` and `"a\nb"` have 2 lines and
  `""` has none. Lines count from 1.
- **`line` and `count`.** `count` added or changed lines start at `line`. Changes with up to
  3 unchanged lines between them form one range, and the range includes those lines.
- **Removed lines.** A range with `count: 0` marks lines that were removed before `line`
  with nothing added in their place. For a removal at the end, `line` is the number of
  lines + 1.
- **`heading`.** The nearest heading at or above `line`, recognized as for `headings`, and
  byte-equal to its entry there. `null` if there is no heading above. For a removal at the
  end, it is the heading of the last line.
- **`headingOccurrence`.** Which heading with this text it is, counting from the top: 1 for
  the first. It is 0 when `heading` is `null`.
- **`offset`.** `line` minus the line of the heading, or `line - 1` without a heading.
- **Order and size.** Ranges are in text order and do not overlap. There are at most 20.
  Beyond that, the two neighbours with the fewest lines between them merge until 20 are
  left, so every change stays covered.
- **`textHash`.** The first 16 hex digits of SHA-256 over the UTF-8 bytes of the text after
  the last edit, with no normalisation. This is the text the backend wrote to the file, which
  is byte-equal to the text of the Automerge document at that moment. For a change on disk,
  it is the file text, which the backend takes over into the Automerge document unchanged.

Clients find a range in the current text, which may have changed since:

1. If the hash of the current Automerge text equals `textHash`, `line` and `count` apply
   as they are.
2. Otherwise they look for the heading `heading` number `headingOccurrence`, or the first
   heading with that text if there are fewer, and take its line + `offset`.
3. Without `heading`, or if no heading with that text exists, they take `line`.

Lines beyond the end are clamped to the text. `ranges[0]` is the place to jump to.

Entries recorded before `feed-ranges` have no `textHash`, and their `ranges` are `null`.
Clients use `headings` for them. An edit that changes only the newline at the end has
`ranges: []`.

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
once per 10 seconds. Clients that are not connected can get a push message instead, see
[push.md](push.md).

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
