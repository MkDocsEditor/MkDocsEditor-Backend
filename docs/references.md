# References, moves and links

This document is the contract between the backend and the clients (web, Android) for
links between pages, the use of resources, and moving or renaming pages, folders and
resources. It extends [`sync-protocol.md`](sync-protocol.md); the websocket protocol stays
version 1, the additions are backwards compatible.

Status: **design, partly implemented**. Implemented: parsing, resolving, the reference
index and its endpoints (section 4), moving and renaming with reference rewriting
(sections 5 and 6, without the late fixes), redirects (section 7), `GET /link/`
(section 8), the `path`, `index`, `mimeType` and `kind` tree fields (section 9), git and
the audit log (section 10). Not yet: late fixes, `movedTo` in broken references, the
`thumbnail` field and endpoint. Where this
document and `openapi-description.yaml` disagree on a REST endpoint, the OpenAPI file wins
once the endpoint is implemented.

Goals:

- Linking between pages and using images and files from anywhere in the wiki is easy:
  the clients offer pickers, and the backend knows which page uses what.
- Pages, folders and resources can be moved and renamed. Every reference to them that
  can be found is updated automatically, by the backend, so web and Android behave the
  same.
- Offline edits are never lost, also not across a move (see
  [Guarantee](sync-protocol.md#guarantee-edits-are-never-lost)).

## Contents

1. [Terms](#1-terms)
2. [Parsing references](#2-parsing-references)
3. [Resolving references](#3-resolving-references)
4. [The reference index and its endpoints](#4-the-reference-index-and-its-endpoints)
5. [Moving and renaming](#5-moving-and-renaming)
6. [Rewriting references](#6-rewriting-references)
7. [Ids after a move: redirects](#7-ids-after-a-move-redirects)
8. [The link path algorithm](#8-the-link-path-algorithm)
9. [Resources in the tree](#9-resources-in-the-tree)
10. [Git, audit log and permissions](#10-git-audit-log-and-permissions)
11. [Errors](#11-errors)
12. [What clients must implement](#12-what-clients-must-implement)
13. [Limits and future work](#13-limits-and-future-work)

## 1. Terms

- **Item:** a section (folder), document (`.md` file) or resource (any other file) of the
  tree. Hidden files, blacklisted paths and `extra_css` files are not items.
- **Path:** the path of an item relative to the docs folder, `/` separated, without a
  leading or trailing slash. The root section has the path `""`. Examples:
  `guide/setup.md`, `guide/img/logo.png`, `guide`.
- **Id:** the REST id of an item. It is derived from the path (as it always was), so it
  changes when the item or a section above it is moved or renamed. The root section has
  the id `root`.
- **Reference:** a place in a document that points to an item: a link, an image, a
  reference definition or an HTML attribute.
- **Destination:** the text of a reference that names the target, exactly as written,
  for example `../img/Strand%20(2).png#top`.

## 2. Parsing references

The backend parses every document for these references:

| Kind | Syntax | Destination |
|---|---|---|
| `link` | `[text](dest)`, `[text](dest "title")`, `[text](<dest with spaces>)` | `dest` |
| `image` | `![alt](dest)`, also with a title or angle brackets | `dest` |
| `definition` | `[id]: dest "title"` at the start of a line (up to 3 spaces), used by `[text][id]` and `![alt][id]` | `dest` |
| `html` | `<a href="…">`, `<img src="…">`, `<source src="…">`, `<video src="…">`, `<audio src="…">`; quotes `"`, `'` or none | the attribute value |

Rules:

- Nested constructs are found separately: `[![logo](img/logo.png)](index.md)` has an
  image and a link.
- Fenced code blocks (```` ``` ```` or `~~~`, also inside lists and admonitions), inline
  code spans, HTML comments and a YAML front matter block at the start of the file are
  skipped. Indented text is **not** treated as a code block: MkDocs content inside
  admonitions (`!!! note`), tabs and lists is indented by four spaces and contains real
  links.
- Footnote definitions (`[^1]: …`) are not reference definitions. Abbreviations
  (`*[HTML]: …`) are not either.
- Destinations with a URL scheme (`https:`, `mailto:`, `data:` and any other
  `letter(letter|digit|+|-|.)*:`) or starting with `//` are external and ignored.
- A destination that is only an anchor (`#heading`) points into the same document. It is
  not a reference to another item and is ignored.
- Everything after the target, the `?query` and the `#anchor`, is kept but plays no role
  in resolving.
- Each reference records its position: `line` (1-based) and `column` (1-based, counted in
  Unicode code points, like the authorship runs) of the first character of the
  destination.

Not parsed: pymdownx snippets (`--8<--`), `.pages` files of the awesome-pages plugin,
wiki links (`[[Page]]`), macros (`{{ … }}`), links built by JavaScript. See
[Limits](#13-limits-and-future-work).

## 3. Resolving references

A destination is resolved the way MkDocs resolves links in Markdown, relative to the
document that contains it:

1. Strip `#anchor` and `?query`. Percent-decode the rest (`%20` is a space, `%C3%A4` is
   `ä`; an invalid escape stays as it is). `+` stays a plus sign.
2. A destination starting with `/` is relative to the docs folder (MkDocs'
   `validation.links.absolute_links: relative_to_docs`). Any other destination is
   relative to the folder of the document.
3. Join and clean the path (`.` and `..` segments). A path that leaves the docs folder is
   **broken** with the reason `outside-docs`.
4. Look the path up in the tree:
   - a document or a resource with exactly this path: the target is that item;
   - a section with this path (written as `folder/` or `folder`): the target is the
     section's index document, `index.md`, or `README.md` if there is no `index.md`
     (MkDocs uses `README.md` as the index then). Without an index document the target
     is the section itself;
   - nothing: **broken** with the reason `not-found`.
5. Matching is exact and case-sensitive, like on the Linux server that runs MkDocs.

A reference is **ok** when it resolves to an item. Whether `#anchor` names a heading of
the target is not checked: heading ids depend on the `toc` extension settings.

URL-style links that only work on the built site with `use_directory_urls` (for example
`../other-page/` meaning `other-page.md`) are reported as broken; MkDocs itself does not
resolve them either.

## 4. The reference index and its endpoints

The backend keeps an index of the references of every document in memory. It is built
in the background at start and kept current:

- Documents that are open (loaded in the sync server) are indexed from their live text,
  so the index follows edits within a second.
- Other documents are indexed from disk and re-read when their size or modification time
  changes (the tree is rebuilt on every change on disk).
- `mkdocs.yml` is read for the paths it uses (see [unused resources](#unused-resources)).

The index is a cache for the read endpoints. A move never relies on it: it parses the
current text of every document while it holds the locks (see [Moving](#5-moving-and-renaming)).

### Objects

**ItemRef**: a short description of an item.

```json
{ "type": "document", "id": "5772684240079201105", "path": "guide/setup.md", "name": "setup" }
```

**Reference**: one reference in a document.

```json
{
  "kind": "image",
  "line": 12,
  "column": 9,
  "destination": "../img/Strand%20(2).png",
  "anchor": "",
  "status": "ok",
  "target": { "type": "resource", "id": "811…", "path": "img/Strand (2).png", "name": "Strand (2).png" }
}
```

- `kind`: `link`, `image`, `definition` or `html`.
- `destination`: as written in the document, including `?query` and `#anchor`.
- `anchor`: the part after `#`, without the `#`, or `""`.
- `status`: `ok` or `broken`.
- `target`: the resolved item (`ok`), otherwise `null`.
- `reason` (`broken` only): `not-found` or `outside-docs`.
- `path` (`broken` only): the resolved path that does not exist, for display.
- `movedTo` (`broken` only, optional): when the path is the old path of an item that was
  moved (see [Redirects](#7-ids-after-a-move-redirects)), the ItemRef of that item now and
  `suggestedDestination`, the destination that would fix the reference. The backend fixes
  such references itself (see [Late fixes](#late-fixes)); the field shows those it has not
  fixed yet.

### Endpoints

All of them are `GET`, so readers may call them.

| Path | Result |
|---|---|
| `/document/{id}/references/` | Outgoing references of a document, in text order: `{"document": ItemRef, "references": [Reference, …]}` |
| `/document/{id}/backlinks/` | References to the document from all documents ("used by") |
| `/section/{id}/backlinks/` | References to the section, its index document, and every item inside it (recursively) |
| `/resource/{id}/backlinks/` | References to the resource |
| `/references/broken/` | All broken references of the wiki |
| `/references/unused-resources/` | Resources that no document and `mkdocs.yml` use |

**Backlinks** answer

```json
{
  "item": { "type": "resource", "id": "811…", "path": "img/logo.png", "name": "logo.png" },
  "backlinks": [
    {
      "source": { "type": "document", "id": "577…", "path": "guide/setup.md", "name": "setup" },
      "kind": "image",
      "line": 12,
      "column": 9,
      "destination": "../img/logo.png",
      "anchor": "",
      "target": { "type": "resource", "id": "811…", "path": "img/logo.png", "name": "logo.png" },
      "internal": false
    }
  ]
}
```

sorted by source path, then line and column. `target` is the item the reference resolves
to (for a section: the section, its index document or an item inside it). `internal` is
true when the source document is inside the section itself (section backlinks only);
those references move along and stay valid. A document's own links to itself (by file
name) are listed too.

**Broken references** answer `{"broken": [ {"source": ItemRef, …Reference fields…} ]}`,
sorted by source path, line and column.

**Unused resources** answer `{"resources": [Resource, …]}` (the tree objects, see
[section 9](#9-resources-in-the-tree)), sorted by path. A resource counts as used when a
reference resolves to it, or when a string value in `mkdocs.yml` equals its path (for
example `theme.logo`, `theme.favicon`, `extra_javascript`). Unused resources are a hint
for cleanup: the backend cannot see templates in `theme.custom_dir`, snippets or links
from outside the wiki.

## 5. Moving and renaming

### Endpoint

`POST /move/` (editors and admins), one item per request:

```json
{
  "type": "section",
  "id": "4108…",
  "parent": "7730…",
  "dryRun": true,
  "updateReferences": true
}
```

- `type`: `section`, `document` or `resource`. `id`: the item.
- `parent`: the id of the target section. Leave it out (or pass the current parent) to
  rename in place.
- `name`: the new name, optional; without it the item keeps its name. Documents are named
  without `.md`, resources with their extension, like the existing create and rename
  endpoints.
- `dryRun`: when true, nothing changes; the answer lists what would change. Default
  false.
- `updateReferences`: default true. With false, only the item is moved; references stay
  as they are (and break). For scripts that rewrite links themselves.

At least one of `parent` and `name` must change something; a move to the same place
answers `400` (`bad-request`, `details.reason: "no-change"`).

The existing rename endpoints (`PUT /section/{id}/`, `PUT /document/{id}/`,
`PUT /resource/{id}/` with `{"name"}`) do the same as a `POST /move/` with only `name`,
including the reference rewriting. Their request and response bodies stay as they are
(the renamed item), so old clients keep working. A preview of a rename is a
`POST /move/` with `name` and `dryRun: true`.

Why one `/move/` endpoint instead of `/resource/{id}/move/`: `POST /resource/{a}/{b}/`
already uploads a file named `{b}` into section `{a}`, so `POST /resource/{id}/move/`
would make a file named `move` impossible to upload.

### Answer

The same object for the preview and the real move (`200`):

```json
{
  "dryRun": false,
  "type": "section",
  "oldId": "4108…",
  "newId": "9921…",
  "oldPath": "guide",
  "newPath": "Handbuch/guide",
  "item": { "type": "section", "id": "9921…", "name": "guide", "path": "Handbuch/guide", "index": "", "subsections": [], "documents": [], "resources": [] },
  "moved": [
    { "type": "section",  "oldId": "4108…", "newId": "9921…", "oldPath": "guide",          "newPath": "Handbuch/guide" },
    { "type": "document", "oldId": "577…",  "newId": "130…",  "oldPath": "guide/setup.md", "newPath": "Handbuch/guide/setup.md" }
  ],
  "documents": [
    {
      "documentId": "577…",
      "newDocumentId": "130…",
      "path": "guide/setup.md",
      "newPath": "Handbuch/guide/setup.md",
      "edits": [
        { "line": 3, "before": "See [the start page](../index.md).", "after": "See [the start page](../../index.md)." }
      ]
    },
    {
      "documentId": "8812…",
      "newDocumentId": "8812…",
      "path": "index.md",
      "newPath": "index.md",
      "edits": [
        { "line": 7, "before": "- [Setup](guide/setup.md#install)", "after": "- [Setup](Handbuch/guide/setup.md#install)" }
      ]
    }
  ],
  "config": [
    { "file": "mkdocs.yml", "line": 9, "before": "  - Setup: guide/setup.md", "after": "  - Setup: Handbuch/guide/setup.md" }
  ],
  "commit": "3f9c2a1b7e40"
}
```

- `item`: the item after the move (absent in a dry run).
- `moved`: every item whose id changes: the item and, for a section, everything inside
  it, sorted by old path. Clients use it to update open editors, tabs and caches.
- `documents`: every document whose text changes, sorted by new path, with the changed
  lines before and after (whole lines, 1-based line numbers of the text before the
  move). A document that only moves without text changes is not listed here.
- `config`: changed lines of `mkdocs.yml` (`nav`, `theme.logo`, `theme.favicon`,
  `extra_javascript`).
- `commit`: the short git commit hash, absent without `git.enabled` or in a dry run.

The real move computes everything again; documents can change between the preview and
the move. Clients show the preview, then send the same request with `dryRun: false`.

### Validation

Before anything changes, the backend checks, in this order, and answers with the first
problem:

| Check | Answer |
|---|---|
| the item and the target section exist | `404` `not-found` |
| the item is not the root section | `400` `bad-request`, `details.reason: "root"` |
| the name is a plain, visible file name (no `/`, `\`, NUL, no leading `.`) | `400` `bad-request`, `details.reason: "invalid-name"` |
| the new path is not blacklisted and not an `extra_css` file | `400` `bad-request`, `details.reason: "blacklisted"` |
| a section is not moved into itself or a section below it | `400` `bad-request`, `details.reason: "into-itself"` |
| something changes | `400` `bad-request`, `details.reason: "no-change"` |
| nothing exists at the new path | `409` `conflict`, `details.reason: "exists"`, `details.path` |

All target sections come from the tree, so a target outside the docs folder cannot be
named.

Moving a document that clients have open is allowed, as renaming is today: the
connections get `document-renamed` and reconnect (see [Redirects](#7-ids-after-a-move-redirects)).

### Order of operations

The move runs under the tree lock and the sync server lock, so no edit, upload or other
tree change interleaves:

1. Validate (above).
2. Flush every loaded document to disk, so disk, Automerge state and index agree.
3. Parse the current text of **every** document (the live text of loaded ones) and build
   the plan: the moved paths, the reference edits per document, the `mkdocs.yml` edits.
   A dry run answers here.
4. Rename the file or folder on disk (one `rename(2)`, atomic). If it fails, nothing has
   changed.
5. Move the Automerge state (sidecar paths) and append the [redirects](#7-ids-after-a-move-redirects).
6. Apply the reference edits to each affected document as an Automerge change (see
   [section 6](#6-rewriting-references)); then write `mkdocs.yml` atomically.
7. Rebuild the tree, send `document-renamed` (close `4009`) to the connections of moved
   documents, send `items-moved` and `tree-changed` on the events websocket.
8. Write the affected documents to disk and commit to git (see
   [section 10](#10-git-audit-log-and-permissions)); write the audit log entry.

If step 5 or 6 fails, the backend undoes what it did: it reverts the edits already
applied (as new Automerge changes that restore the old text, so clients that synced them
in the meantime converge), restores `mkdocs.yml`, moves the state back and renames the
file back. The request then fails with `500` and nothing is half done. Failures in step 8
(a git commit) do not undo the move; they are logged, as for every other tree change.

## 6. Rewriting references

### Which references change

For every document `D` and every **ok** reference in it, with the source path `P` (the
path of `D`) and the target path `T` (the path as resolved in step 3 of
[Resolving](#3-resolving-references), before the index lookup):

- map both through the move: `P' = map(P)`, `T' = map(T)`, where `map` replaces the old
  path of the moved item with the new one (for a section: as a prefix, segment-wise);
- compute the new destination from `P'` to `T'` (below);
- if the new destination resolves to the same target as the old one did (compared
  decoded and cleaned, so `./a.md` and `a.md` are equal), the reference stays untouched.

This covers references to the moved item from everywhere, references inside the moved
item to the outside (relative paths change when the source moves), and references
between items that move together (they stay as they are). Broken references are left
alone, except for the [late fixes](#late-fixes).

A folder link (`folder/`) follows the folder: if only `folder/index.md` moves, the folder
link stays and now points to the folder without its index document.

### Keeping the style

Only the destination's target path is replaced, nothing else of the markup: text, alt
text, title, angle brackets, HTML quotes and attributes, `{ width=300 }` attribute lists,
`?query` and `#anchor` stay as they were.

- A relative destination stays relative, computed from the document's new folder with
  the [link path algorithm](#8-the-link-path-algorithm). A leading `./` is kept if it
  was there.
- An absolute destination (`/…`) stays absolute: `/` plus the new path.
- A folder destination keeps its trailing slash (or its absence).
- Path segments that the old destination already had keep their spelling: `Strand%20(2).png`
  stays like that and does not become `Strand%20%282%29.png`.
- New segments are encoded with the algorithm. Two exceptions keep the author's style: if
  the old destination percent-encoded a non-ASCII character (`%C3%A4`), non-ASCII
  characters are encoded too; if the old destination contained a raw space (allowed inside
  `<…>` and in HTML attributes), spaces stay raw.
- In an HTML attribute that wrote `&` as `&amp;`, the new value does so too.

### Through Automerge

The edits of a document are applied as one Automerge change by a fresh actor bound to the
pseudo user `:server` (like a restore), as splices of only the changed destinations,
under the document's lock and on its current text. So:

- connected clients receive the change with the next sync message, like an edit by
  another user;
- offline clients merge it when they reconnect. Their offline edits stay; Automerge
  merges the server change and theirs like two users' edits. If an offline client edited
  the very same destination, both edits are kept character by character, and the result
  may be a broken reference, which then shows up in `/references/broken/` (and is fixed
  later if it names an old path, see below). No text is lost.
- The change counts for the moving user in git attribution.

Documents that are not loaded are loaded for the edit (creating their Automerge state if
they had none), as a restore does.

### Late fixes

An offline client can add a reference to an item's old path after the item moved, or
merge an offline edit that reintroduces an old path. When the backend writes a document
to disk (at most two seconds after an edit), it checks its broken references. A broken
reference whose path is the old path of a moved item (from the [redirects](#7-ids-after-a-move-redirects))
and whose item still exists is rewritten to the current path, as a `:server` change like
above. Such fixes are committed with the next edit commit of the document. A path that
exists again (a new item was created there) is not broken and never rewritten.

## 7. Ids after a move: redirects

Ids are derived from paths, so a move or rename changes the id of the item and of
everything inside a moved section. Connected clients learn the new id from
`document-renamed`. Offline clients, other tabs and bookmarks still hold the old id. The
backend keeps a durable redirect table for them.

### The redirect table

`<stateDir>/redirects.json`, a list that only grows (like the Automerge state, nothing is
ever removed):

```json
[
  {
    "time": "2026-11-01T10:00:00Z",
    "type": "document",
    "oldPath": "guide/setup.md",
    "newPath": "Handbuch/guide/setup.md",
    "uuid": "9f2c…",
    "epoch": "b1c2…",
    "accountId": "k3x8m2q9w4e7r1t6y5u0i3o8pa"
  }
]
```

One entry per moved item (for a section: the section and every item inside it). `uuid`
and `epoch` identify the Automerge lineage of a document that has saved state (see
[Storage](sync-protocol.md#9-storage)); documents without state have neither (no client
can hold an Automerge copy of them).

**Resolving an old id** (`X`): find the entries whose `oldPath` has the id `X`, newest
first.

- A document entry with a `uuid` resolves through the state store: the document whose
  state has this `uuid`, at its current path, wherever it moved since. If that state is in
  `documents/deleted/`, the document was deleted.
- Other entries follow the chain of later moves of `newPath` (a later entry whose
  `oldPath` is this `newPath`), up to the current path.
- The result is the item at the resolved path, if it exists.

An id that names an existing item is never redirected by REST: if a new item was created
at an old path, the old id belongs to it.

### REST

`GET /moved/{id}/` (readers too): `200` with
`{"oldId": "…", "type": "document", "item": ItemRef, "movedAt": "<RFC 3339>"}` when `{id}`
is an old id of an item that still exists, otherwise `404`. A current id also answers
`404`: it has not moved.

Every `404` (`not-found`) of an item endpoint for an id that has a redirect carries it:

```json
{ "name": "Not found", "code": "not-found", "message": "No item with id '577…' found",
  "details": { "movedTo": { "type": "document", "id": "130…", "path": "Handbuch/guide/setup.md", "name": "setup" } } }
```

### Document websocket

The client connects to `GET /document/{id}/ws/` with the id it knows and sends `hello`
with its `epoch`, as today. The server decides after authenticating the `hello`:

1. **The id names no document, and it has a redirect:** the server upgrades, accepts the
   `hello`, then sends `document-renamed` with the current id and closes with `4009`.
   (Without a redirect it still answers `404` before the upgrade.)
2. **The id names a document, and `hello.epoch` is the epoch of that document**, or the
   `hello` has no epoch: normal connection.
3. **The id names a document, `hello.epoch` is set but differs, and a redirect of this id
   leads to an existing document whose epoch is `hello.epoch`:** the client holds the old
   document that moved away, and a new document was created at its old path. The server
   sends `document-renamed` pointing to the moved document and closes with `4009`.
   Without this check the client would apply its unsynced edits to the wrong document.
4. Otherwise the normal connection with the epoch handling of
   [sync-protocol.md](sync-protocol.md#epochs-and-offline-edits) (the server lost its
   state).

`document-renamed` is unchanged:
`{"type":"document-renamed","documentId":"<old>","newDocumentId":"<new>"}`. The client
reconnects to `newDocumentId` with the same `epoch` and keeps its document. The Automerge
state moved with the file, so the epoch matches and the offline edits merge with a normal
sync, including the server's reference rewrites.

### Events websocket

Before `tree-changed`, a move sends

```json
{ "type": "items-moved", "moves": [ { "type": "document", "oldId": "577…", "newId": "130…", "oldPath": "guide/setup.md", "newPath": "Handbuch/guide/setup.md" } ] }
```

with the `moved` list of the move answer. Clients that missed it (disconnected) still
fetch the tree after reconnecting and learn about old ids through the redirects above.

### Client rules

- Persist documents for offline use with their id **and** epoch (they already must).
- On `document-renamed` (live or right after `hello`), on `items-moved`, and on a `404`
  with `details.movedTo`: re-key everything stored under the old id (Automerge document,
  server heads, actor id, open tabs, drafts, recent lists) to the new id, then reconnect.
  Never drop the local document because its id is unknown.
- A client that holds an id that answers `404` without `movedTo`, and a websocket that
  closes with `4004`, means the document is gone: keep the local copy and offer to save it
  (as `document-deleted` says today).

## 8. The link path algorithm

The clients' link and image pickers compute the destination themselves, and the backend
uses the same algorithm to rewrite references. It is specified here with test vectors in
[`link-paths.json`](link-paths.json), which the backend's tests run and the clients' tests
must run too.

**Decision: a documented algorithm that both clients implement, plus an endpoint.** The
pickers run on every selection while the user browses the tree, and Android edits offline:
an endpoint would add a round trip per click and would not work offline at all. The
algorithm is twenty lines and needs only the tree, which the clients have anyway (the tree
now carries paths, see [section 9](#9-resources-in-the-tree)). The endpoint
`GET /link/?from={documentId}&to={itemId}&anchor={anchor}` (readers too) returns the same
result as the reference implementation, for scripts and for checking a client against the
server:

```json
{ "destination": "../img/Strand%20%282%29.png", "markdown": "![Strand (2)](../img/Strand%20%282%29.png)", "target": ItemRef }
```

`markdown` is `![name without extension](dest)` for images, `[document name](dest)` for
documents and sections, `[file name](dest)` for other resources.

### Input

- `from`: the path of the document that will contain the link, for example
  `guide/setup.md`.
- `target`: `type` and `path` of the item; for a section also `index`, the path of its
  index document (`index.md`, else `README.md`) or empty.
- `anchor`: optional, without `#`.

### Steps

1. **Target path.** A document or resource: its path. A section with an index document:
   the index document's path. A section without one: the section path, and remember
   `folder = true`.
2. **Same document.** If the target path equals `from`: with an anchor the result is
   `#` + encoded anchor; without one it is the encoded file name of `from`. Stop.
3. **Split** the folder of `from` (everything before the last `/`, `""` for a top-level
   document) and the target path into segments on `/`. `""` gives no segments.
4. **Common prefix.** Count the leading segments that are equal (exact, case-sensitive,
   segment-wise: `guide` and `guidelines` differ).
5. **Build:** one `..` for every segment of the `from` folder after the common prefix,
   then the target's segments after the common prefix, each segment encoded (below),
   joined with `/`.
6. **Folder:** if `folder`, append `/`; if the result is then empty or `/`, it is `./`.
7. **Scheme guard:** if the first segment contains `:`, prepend `./` (so `a:b.md` is not
   read as a URL scheme).
8. **Anchor:** if given, append `#` and the encoded anchor.

### Encoding

Percent-encode (UTF-8 bytes, upper-case hex) every byte that is a control character or
space (`0x00`–`0x20`, `0x7F`) or one of these ASCII characters:

```
"  #  %  '  (  )  <  >  ?  [  \  ]  ^  `  {  |  }
```

Everything else stays as it is, including letters with diacritics and all other non-ASCII
characters (`Käsekuchen.md` stays readable), and `& + , ; = : @ ! $ * ~`. Parentheses are
encoded although balanced ones would be allowed, so a destination never needs angle
brackets.

### Examples

| from | target | anchor | destination |
|---|---|---|---|
| `guide/setup.md` | document `guide/install.md` | | `install.md` |
| `guide/setup.md` | document `index.md` | | `../index.md` |
| `guide/setup.md` | resource `img/Strand (2).png` | | `../img/Strand%20%282%29.png` |
| `Rezepte/Kuchen.md` | document `Rezepte/Käsekuchen.md` | | `Käsekuchen.md` |
| `guide/setup.md` | document `guide/setup.md` | `step-2` | `#step-2` |
| `guide/setup.md` | section `reference` (no index) | | `../reference/` |
| `guide/setup.md` | document `guidelines/x.md` | | `../guidelines/x.md` |

The full list is in [`link-paths.json`](link-paths.json).

## 9. Resources in the tree

The tree objects (`GET /section/`, `GET /section/{id}/`, `GET /document/{id}/`,
`GET /resource/{id}/` and the items in other answers) gain these fields:

| Object | Field | Meaning |
|---|---|---|
| Section, Document, Resource | `path` | the path relative to the docs folder (section 1), the input of the link path algorithm |
| Section | `index` | the id of the section's index document (`index.md`, else `README.md`), or `""` |
| Resource | `mimeType` | from the file extension, `application/octet-stream` when unknown |
| Resource | `kind` | `image`, `video`, `audio`, `pdf`, `text`, `archive` or `other`, for icons and pickers |
| Resource | `thumbnail` | true when `GET /resource/{id}/thumbnail/` works for it |

Existing fields (`filesize`, `modtime`, `url` of documents) stay.

`kind` by MIME type: `image/*` is `image`, `video/*` `video`, `audio/*` `audio`,
`application/pdf` `pdf`, `text/*`, `application/json`, `application/xml`,
`application/yaml` and `application/toml` `text`, `application/zip`, `application/gzip`, `application/x-tar`,
`application/x-7z-compressed` and `application/vnd.rar` `archive`, everything else
`other`.

`GET /resource/{id}/thumbnail/?size=256` (readers too) returns a JPEG (PNG for images
with transparency) whose longer side is at most `size` pixels (64 to 1024, default 256,
rounded up to 64, 128, 256, 512 or 1024). It works for PNG, JPEG, GIF (first frame) and
WebP images up to 50 megapixels, otherwise `404`. Smaller images are returned scaled to
themselves (re-encoded, not upscaled). Thumbnails are cached in `<stateDir>/thumbnails/`
by path, size and modification time; the answer has `Cache-Control: private, max-age=86400`
and an `ETag`. SVG images get `thumbnail: false`: clients show them directly from
`/resource/{id}/content/`.

## 10. Git, audit log and permissions

### Git

With `git.enabled`, a move is committed right away, in one commit, authored by the user
who moved (committer: the backend):

- the old and the new path, so git records a rename (`git mv` semantics: the history of
  the file follows it, `git log --follow` and the history endpoints find the old commits);
- every document whose references were rewritten, and their pending edits (their authors
  become `Co-authored-by:` trailers, as for other tree changes);
- `mkdocs.yml` when its `nav` changed, if it is inside the repository.

Message:

```
Move guide to Handbuch/guide

Update references in 2 documents:
- Handbuch/guide/setup.md
- index.md
- mkdocs.yml

Co-authored-by: Anna <anna@example.org>
```

`mkdocs.yml` is listed when it changed, but not counted. A rename in place (same parent
section) says `Rename … to …` as today. The rewritten documents are written to
disk before the commit, so the commit holds the rewritten text. A reference change keeps
far more than half of a file the same, so git's rename detection (50 % similarity) keeps
pairing the old and the new path.

### Audit log

Every real move writes an audit event (see [auth.md](auth.md#audit-event)):
`item.moved` for a move into another section, `item.renamed` for a rename in place, with
`actorId` the user and `details`:
`{"type": "section", "oldPath": "guide", "newPath": "Handbuch/guide", "items": 12, "documents": 2}`
(`items`: number of moved items, `documents`: number of documents with rewritten
references). Dry runs are not logged.

### Permissions

| Action | reader | editor | admin |
|---|---|---|---|
| references, backlinks, broken references, unused resources, link, moved, thumbnail | yes | yes | yes |
| `POST /move/` (also with `dryRun`), rename | no (`403`) | yes | yes |

## 11. Errors

As everywhere (see [auth.md](auth.md#10-errors)): `{"name", "code", "message", "details"}`.
Clients switch on `code` and `details.reason`.

| HTTP | `code` | `details` | When |
|---|---|---|---|
| 400 | `bad-request` | `reason`: `invalid-name`, `blacklisted`, `into-itself`, `root`, `no-change`, `invalid-type` | invalid move |
| 403 | `forbidden` | | readers |
| 404 | `not-found` | `movedTo` (optional, see section 7) | unknown item or target section |
| 409 | `conflict` | `reason: "exists"`, `path` | the new path exists |
| 500 | `internal` | | the move failed and was undone |

## 12. What clients must implement

**Both:**

- **Paths:** use the new `path` fields of the tree; keep using ids for API calls.
- **Link picker** (editor toolbar, and on typing `[`/`](` if wanted): pick a document,
  section (its index) or resource from the tree, optionally an anchor (headings of the
  target from `GET /document/{id}/content/`), insert `[text](dest)` with the
  [link path algorithm](#8-the-link-path-algorithm). Run the test vectors of
  `link-paths.json` in the client's tests.
- **Image and file picker:** the same for resources, inserting `![alt](dest)` for
  `kind == "image"`, with thumbnails (`thumbnail == true`) or the content URL; offer
  "upload here" into a section and insert the link to the new resource.
- **Backlinks ("used by")** for documents, sections and resources, linking to the source
  line. Show them in the delete confirmation too ("3 pages use this image").
- **Outgoing references** of the open document, with broken ones highlighted.
- **Move:** a "move to…" action with a section picker and an optional new name (and
  drag and drop in the tree, if the platform supports it). First `POST /move/` with
  `dryRun: true`, show the affected documents and lines, then the same request with
  `dryRun: false`. Rename dialogs may show the same preview.
- **After a move:** handle `items-moved` (re-key open editors, tabs and caches, update the
  current route), then refetch the tree on `tree-changed`.
- **Redirects:** on `document-renamed` (also right after `hello`), and on a `404` with
  `details.movedTo`, re-key all local data of the old id to the new id and continue. Never
  discard a local document because its id is unknown.
- **Broken references and unused resources** views (an admin or maintenance screen is
  enough).

**Web:** keep the route (`/document/{id}`) current: replace the URL on a re-key, and
resolve bookmarked old ids through `GET /moved/{id}/`.

**Android:** for every document held offline, the next connection uses the stored id and
epoch; follow `document-renamed` and re-key the stored copy before syncing. Run the
re-key in the same transaction as the update of the stored document, so a crash in between
cannot lose the copy.

## 13. Limits and future work

- Anchors are not checked against headings, and a renamed heading does not update
  `#anchor` references.
- Not parsed: snippets (`--8<--`), `.pages` files, macros, raw HTML other than the listed
  tags, links in `theme.custom_dir` templates.
- References written in a URL style that only works with `use_directory_urls` are
  reported as broken and not rewritten.
- One item per move; moving several items is several requests (each one atomic).
- The redirect table only grows; at about 200 bytes per moved item that is fine for years
  of wiki use.
