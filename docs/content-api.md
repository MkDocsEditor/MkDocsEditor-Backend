# Content API: reading, editing and searching documents over REST

Scripts and agents (LLMs, through the MCP server or directly) change documents over REST,
next to people who type in the editor at the same time. These endpoints never write the
file: they change the live Automerge document of the page, like a websocket client does.
Connected clients see the change right away, and concurrent typing merges. The file, the
feed and git follow through the usual flush and commit logic
([sync-protocol.md](sync-protocol.md) sections 9 and 10).

All paths are under `/api`. Every request authenticates like any other request
([auth.md](auth.md)), usually with an app token: `Authorization: Bearer mdp_…`. Reading
needs any role or token scope. Changing needs the role `editor` or `admin`, and for app
tokens the scope `write`.

## 1. Revisions

A **revision** is a version of a document: its Automerge heads (change hashes, hex), sorted
and joined with `.`, for example `3f9a…c2.8b1d…07`. A client gets the current revision
from the `ETag` header of `GET /document/{id}/content/`, or from the answer of a change.
It sends the revision back as `baseRevision` to say which text a change refers to.

A revision stays valid as long as the document keeps its Automerge history. After a new
epoch (lost or damaged state, [sync-protocol.md](sync-protocol.md) section 5), old
revisions are unknown. A change based on one is refused with `409 unknown-revision`.
The client then reads the document again.

## 2. Reading

`GET /document/{id}/content/` returns the markdown as `text/plain`, with the revision as
the `ETag` header (in quotes). For an open document this is the live text, which can be
newer than the file. Files that are not valid UTF-8 cannot be edited. They come back as
they are, without an `ETag`.

## 3. Edits by search strings

`POST /document/{id}/edits/`

```json
{
  "edits": [
    { "old": "Some text.", "new": "Other text." },
    { "old": "## Old heading", "new": "## New heading" }
  ],
  "baseRevision": "3f9a…c2"
}
```

- The edits apply in order. Each `old` must occur **exactly once** in the text that the
  edits before it left. Overlapping occurrences count: `aa` occurs twice in `aaa`.
- Either all edits apply or none.
- `baseRevision` is optional:
  - Without it, the edits match against the current text.
  - With it, they match against the text of that revision, and the result is merged with
    everything that changed since. An agent that read the page a while ago can edit what
    it saw, even if someone has typed elsewhere since.
- `old` may be empty only in an empty document. That is how a script fills a new page.
- There are 1 to 100 edits per request.

Answer `200`: `{"revision": "…"}` and the same value as `ETag`. Edits that change nothing
create no change, and the revision stays the same.

| Status | `code` | Meaning |
|---|---|---|
| 409 | `edit-not-found` | the `old` of edit `details.index` does not occur |
| 409 | `edit-ambiguous` | the `old` of edit `details.index` occurs more than once: add context |
| 409 | `unknown-revision` | the document does not know `baseRevision`; read it again |
| 400 | `bad-request` | no edits or too many, an empty `old` (`details.reason: "empty-old"`), an invalid revision |
| 403 | `forbidden` | a reader, or an app token with the scope `read` |
| 413 | `payload-too-large` | the body is larger than 4 MiB |
| 429 | `rate-limited` | see [Limits](#6-limits); `Retry-After` says when to retry |

## 4. Writing the whole text

`PUT /document/{id}/content/`

```json
{ "content": "# Setup\n\nThe full new text.\n", "baseRevision": "3f9a…c2" }
```

`baseRevision` is required. Without it the answer is `428 base-revision-required`.
Overwriting a page without knowing what it contained would lose concurrent work.

The write is a **three-way merge**:

1. The server forks the document at `baseRevision`.
2. It turns the difference between the text there and `content` into splices
   (diff-match-patch, the same as for external changes) and applies them on the fork.
3. It merges the fork into the live document.

What others changed since `baseRevision` stays, unless the new content changes the same
place. Then both changes stay, interleaved by Automerge, as with any concurrent edit.

Answer and errors are the same as for edits, minus the edit codes. For small changes,
edits are better: they send less and match what the agent meant more exactly.

## 5. Attribution

The server applies a change with an Automerge actor of the requesting **account**, so it
counts as the account's own edit in authorship, the feed and git. When the request used
an app token, the change is also marked as an **agent** edit:

- **Actor map**: the actor is bound to the account, and its binding keeps the id and the
  name of the token. `GET /actors/`, the `actors` of `initial-content` and the `actor`
  messages carry it as `agent`:

  ```json
  { "actorId": "…", "accountId": "…", "username": "markus", "displayName": "Markus", "color": "#e8590c", "avatar": null,
    "agent": { "tokenId": "…", "name": "claude" } }
  ```

  The name is the token's name when the actor was created, so it stays known after the
  token is revoked. Authorship runs name the actor, so clients can show agent text with
  the `agent` of the actor map.
- **Git**: the commit is authored by the account as usual. For each account and app
  token, a trailer follows the `Co-authored-by` trailers:

  ```
  Update guide/setup.md

  Via-App-Token: claude (Markus <markus@example.com>)
  ```

  `GET /document/{id}/history/` lists the token names of a commit as `appTokens`.

An agent keeps its actor while the document stays loaded, so a loop of edits does not
create an actor per request. The server takes a new actor when the document was unloaded,
or when an edit is based on a revision older than the agent's last change. Automerge does
not allow two concurrent changes with the same actor.

Changes over REST count for the commit triggers like typing does
([sync-protocol.md](sync-protocol.md) section 10). A document that changes every few
seconds is committed after `git.commitMaxInterval`, or after `git.commitAfterIdle`
without changes. An agent loop therefore does not create a commit per edit.

## 6. Limits

- Requests of at most 4 MiB, at most 100 edits.
- At most 60 content changes (edits or `PUT`) in a burst, then one per second, per app
  token. Requests with a session count per account. Beyond that the answer is `429` with
  `Retry-After`. `server.auth.rateLimits.disabled` turns this off, like the other limits.

## 7. Search

`GET /search/?q=…&limit=…` finds documents that contain **every word** of `q`
(case-insensitive) in their text, title or path. Open documents are searched with their
live text. The best matches come first: a word in the title or path weighs ten times a
word in the text. `limit` is 1 to 100, default 20.

```json
{
  "results": [
    { "id": "1753…", "path": "guide/setup.md", "title": "Setup",
      "snippets": [ { "line": 3, "text": "Some text about the setup." } ] }
  ],
  "total": 1
}
```

`total` counts every match, also those beyond `limit`. `snippets` are the first three
lines with a match, shortened to 160 characters around it.
