# MCP server: the wiki as tools for LLM agents

The backend has a [Model Context Protocol](https://modelcontextprotocol.io) server at
`/api/mcp`. Claude Code and other MCP clients use it to search, read and edit the wiki's pages.
The tools are thin wrappers over the content API ([content-api.md](content-api.md)):
- Changes go into the live Automerge document. People with the page open see them right away,
  and concurrent typing merges.
- Changes count as the account's own edits, marked as made by an agent.

## 1. Connecting

With OAuth (see [oauth.md](oauth.md)), add the server without a token:

```sh
claude mcp add --transport http wiki https://wiki.markusressel.de/api/mcp
```

1. In Claude Code, run `/mcp` and choose the server to log in.
2. The browser opens the web app. Approve the request there, with `write` to let the agent
   edit or `read` to let it only read.
3. The grant appears in Settings, App tokens, with the client's name.

Without OAuth, use an app token:

1. Create an app token in the web app: Settings, App tokens. Use the scope `write` to let
   the agent edit, `read` to let it only read.
2. Add the server with the token:

   ```sh
   claude mcp add --transport http wiki https://wiki.markusressel.de/api/mcp --header "Authorization: Bearer mdp_…"
   ```

   Other clients take the URL `https://<host>/api/mcp` and the same header.

Revoking the token or grant disconnects the agent. A password change revokes all app tokens
and grants of the account.

## 2. Transport and authentication

- MCP streamable HTTP transport, built with the official Go SDK
  (`github.com/modelcontextprotocol/go-sdk`), up to protocol revision 2026-07-28.
- The server keeps no sessions: every request is a `POST` with a JSON-RPC message, the
  answer is JSON, and there is no `Mcp-Session-Id`. `GET` and `DELETE` answer `405`.
- Every request needs an `Authorization` header. It can hold:
  - an OAuth access token (`Bearer mdo_…`, [oauth.md](oauth.md))
  - an app token (`Bearer mdp_…`)
  - a session token (`Bearer mds_…`)
- Without a header the answer is `401`, with `WWW-Authenticate` pointing to the OAuth
  metadata. The session cookie is not accepted, so a web page cannot call the server in the
  name of a signed-in user.
- Readers and app tokens with the scope `read` get only the read tools.

## 3. Tools

Pages are named by their path relative to the wiki root (`guide/setup.md`; `.md` and a
leading `/` are optional) or by their id. Sections are named by their path, and empty or `/`
means the top level.

| Tool | Write | What it does |
|---|---|---|
| `search` | | Full-text search, see [content-api.md](content-api.md) section 7: paths, titles, matching lines |
| `list_tree` | | Paths and titles of all pages, or of those below `section`. With `files`, also the sections and the files that are no pages, with size and type |
| `read_document` | | The markdown of a page with a header: `path`, `title`, `revision`, `viewers`, and where the viewers are |
| `read_presence` | | Where the people who have a page open are: cursor, selection and visible lines, mapped to the current revision. See section 4 |
| `point_at` | | Moves the agent's cursor, selection and view in a page people have open, changes no text. See section 4 |
| `read_resource` | | A file that is no page, like an image a page links to. See below |
| `download_resource` | | A file unchanged, as an embedded resource. See below |
| `get_backlinks` | | Pages that link to a page, with line and link text |
| `changes_since` | | What changed on a page since the caller last read it (or since `since`, a revision), as a diff with who changed it. See below |
| `read_history` | | The commits of a page, newest first (`limit`, default 20, at most 100): revision, time, authors, agents, lines added and removed, headings. Only with git |
| `read_revision` | | The markdown of a page at a commit from `read_history`. Only with git |
| `blame` | | Who last changed each line of a page, as blocks with commit, time, author and agents, like `GET /document/{id}/blame/`. Only with git |
| `check_links` | | Broken links and files nothing links to (`GET /references/broken/`, `GET /references/unused-resources/`), optionally below `section` |
| `link_to` | | The link from a page to a page, file or section: `destination` and `markdown`, like `GET /link/` |
| `recent_changes` | | The change feed of the wiki, newest first (`limit`, default 30, at most 200; `before`, a cursor), like `GET /feed/` |
| `edit_document` | yes | Replaces text: each `old` must occur exactly once. Optional `baseRevision`. Like `POST /document/{id}/edits/` |
| `write_document` | yes | Replaces the whole text against a required `baseRevision`, merged three-way. Like `PUT /document/{id}/content/` |
| `create_document` | yes | Creates a page in `section` with the file name `name` (without `.md`), optionally with `content` |
| `move_document` | yes | Moves and/or renames a page. Links in other pages are updated, like `POST /move/` |
| `delete_document` | yes | Deletes a page; `force` when people have it open. Lists the links that broke |
| `upload_resource` | yes | Creates or replaces (`replace`) a file that is no page. See below |
| `request_upload` | yes | A one-time URL to upload a larger file with `curl`. See below |
| `move_resource` | yes | Moves and/or renames a file; links are updated |
| `delete_resource` | yes | Deletes a file. Lists the links that broke |
| `create_section` | yes | Creates a section and missing parent sections |
| `move_section` | yes | Moves and/or renames a section with its content; links are updated |
| `delete_section` | yes | Deletes a section: `recursive` if it is not empty, `force` when people have one of its pages open. Lists the links that broke |
| `revert_change` | yes | Undoes a commit made with the caller's own app token, keeping later edits, like `POST /document/{id}/history/{revision}/revert/`. Only with git and an app token |

`read_resource` takes the file's path relative to the wiki root, its id, or a link destination
as written in a page together with that page as `document` (`img/photo%201.jpg` from
`guide/setup.md` is `guide/img/photo 1.jpg`):
- PNG, JPEG, GIF and WebP come back as image content, scaled so that the longer side is at most
  `size` pixels (64 to 1024, default 1024, rounded up like the thumbnails of
  [references.md](references.md) section 9) and with the EXIF orientation applied. They share the thumbnail
  cache. A text content before it has `path`, `mimeType` and `size`, and what the original
  holds beyond the picture:
  - `original`: its dimensions (upright), and the size it was scaled to
  - `camera`: Make and Model from Exif
  - `metadata`: the JPEG APPn segments and PNG chunks with their identifier and size, consecutive
    ones of a kind merged (`APP3 x40 (2.5 MiB)`)
  - `after the end of the image`: bytes after JPEG EOI or PNG IEND
  - `note`: when 64 KiB or more are neither the picture nor ordinary metadata (JFIF, Exif, ICC,
    XMP, Photoshop, Adobe, small PNG text chunks), a hint to `download_resource`. Thermal cameras,
    for example, store their raw values like this.
- Text files (and SVG) up to 256 KiB come back as text, after the same header and `---`.
- Other files are a tool error.

`download_resource` takes the same names and returns the file unchanged, up to 64 MiB, as an
embedded resource (`blob`, URI `wiki:///<path>`) after a text content with `path`, `mimeType` and
`size`. Claude Code (2.1.69 and later) saves such blobs to a file and gives the agent its path,
but forwards image blobs as images: images are therefore sent as `application/octet-stream`, the
text names their real type.

`upload_resource` takes the path of the file like `read_resource` (from the wiki root, or relative
to `document`), and the content as `text` or as `base64`. A request is limited to 4 MiB, so
binary files must be small (larger ones: `request_upload`). Missing sections are created. An existing file is only replaced
with `replace`; pages (`.md`) are refused, they are created with `create_document`. The commit
is `Upload <path>` or `Replace <path>`.

`request_upload` takes the same `path`, `document` and `replace`, and answers with a command like
`curl -sS -T <file> 'https://…/api/uploads/<secret>/'`. The agent runs it in its shell, so the bytes of
a larger file do not pass through the model. `PUT /uploads/{token}/` takes the body as the file
(`server.maxUploadBytes`, default 64 MiB; the body goes to a temporary file `.upload-*` in the docs folder
first, not into memory, so a limit of 1 GiB or more is fine for the backend) and does what `upload_resource` does, with the same commit message and the
rate limit of the content API, as the account and app token that asked:
- A reverse proxy in front has limits of its own (nginx: `client_max_body_size`, default 1 MiB).
- The secret in the URL is the credential, the request needs no login. It works once (a failed
  attempt uses it up), for the path it was issued for, for 10 minutes. An account has at most 10
  open URLs.
- The tickets are kept in memory: a restart invalidates them.
- The account must still be allowed to write when the file arrives, and the path is checked
  again (a file that appeared meanwhile needs `replace`).
- The access log shows the URL; it is useless after its first use.

The delete tools answer the links from outside the deleted item that broke, as
`path:line destination`, for the agent to fix them. Deleting is committed like
`DELETE /document/{id}/` and the other delete endpoints: the files stay in the git history.

Not offered as tools: accounts, administration, pins, push subscriptions and QR codes (personal
or admin matters), the restore of a page revision (`revert_change` undoes the agent's own
commits; `read_revision` and `write_document` restore anything else), and the history of files
that are no pages, which the backend only has for pages.

The tools exist because agents cannot use `GET /resource/{id}/content/`: OAuth access tokens
only work for the MCP endpoint ([oauth.md](oauth.md)), and the agent does not see its token.

`changes_since` keeps what each app token saw of each page, apart from the token's person
(the `seen` marks of [sync-protocol.md](sync-protocol.md) section 11 under the key
`token:<id>`):
- `read_document` and `changes_since` mark the text they returned as seen. The first
  `changes_since` of a page only says that there is nothing to compare with yet.
- `edit_document` and `write_document` move the mark over the agent's own change only when
  the agent had seen the text it changed; otherwise the next `changes_since` also shows the
  edits of others it never read, together with its own.
- The output lists `changed by` (accounts, with their agents) and the hunks like a unified
  diff with the heading of each hunk.

`revert_change` refuses commits with other authors than the caller's account and app token
(by name): an agent can undo its own mistake, not the work of people or other agents. Since
agent edits get commits of their own, each of its edits is one commit to revert. Conflicts
with later edits change nothing and name the headings.

Errors are tool results with `isError` and a message the agent can act on, for example
"edit 0: old occurs more than once in the text, add context to make it unique".

Content changes share the rate limit of the content API: a burst of 60, then one per
second, per app token. Creating, uploading, moving and deleting count too.

## 4. Agents in the editor

An agent that changes a page shows up for the people who have it open:

- **Presence**: a `presence` message as from any peer, with the account's name, colour and
  avatar, plus:
  - `"agent": {"tokenId": "…", "name": "<token name>"}`
  - `deviceName` set to the token name
  - `selections: []`: automerge-go cannot create Automerge cursors, so an agent has no cursor
    of its own; `agent.focus` says where it is (below)
  - `clientId` `agent-<12 hex>` and a stable `instanceId` derived from the token

  The agent is also part of the `peers` of `presence-snapshot` for clients that connect
  later. A `presence-leave` follows 60 seconds after its last change, 5 minutes with a focus.
- **Focus**: every change puts the agent's cursor at the end of what it inserted, and
  `point_at` moves its cursor, selection and viewport: the `focus` of
  [sync-protocol.md](sync-protocol.md#agents). People who follow the agent are taken there.
- **Live edits**: `edit_document` and `write_document` take `live` (default false). The change
  is the same (atomic, the new `revision` in the answer, the same rate limit), and the clients
  show the inserted text as typed (`agent-change` in sync-protocol.md). The tool descriptions
  advise: without viewers change instantly; with viewers and a small or medium change, consider
  `live`. Every caller sees `live`; with a session token the call fails with "live needs an
  app token" and changes nothing. The REST endpoints and `create_document` have no `live`.
- **Authorship and history**: the agent's actor has `agent` in the actor map. Commits carry
  a `Via-App-Token` trailer, and history entries list the tokens in `appTokens`. See
  [content-api.md](content-api.md) section 5.

Changes over a session token (`mds_…`) are no agent changes: they appear as the account's
own, without presence. `point_at` and `live` need an app token (OAuth grants are app tokens). One app
token used by two agent sessions at once shares one presence and one focus.

**`viewers`**: so that the agent can choose how to show a change, `read_document` (a header
line) and the answers of `edit_document`, `write_document` and `point_at` list who has the page
open: the display names of the accounts with a connection to it, sorted, the caller's own
account marked `(you)`. Agents and ghosts are not in it.

**Where people are**: `read_presence` and a block of `read_document` tell the agent where the
people who have the page open are, from the indexes their clients send with their presence
(`at` in [sync-protocol.md](sync-protocol.md#indexes)), mapped to the current revision:

- One entry per connection of a person, at most 10, the most recently active first. Agents are
  not included. The caller's own account is, marked `(you)`: the person who asks "where am
  I?" has the page open with it. Ghosts are not included: only people who have the
  page open now.
- Per entry: the display name, the device name if there is one, and `updated`, the seconds
  since the last presence of the connection (`lastActiveAt`).
- The cursor: line and column (1-based, columns in code points) and the code point index, at
  the revision of the answer. The heading the cursor is under (the nearest ATX heading before
  it, like the sections of `point_at`), if any.
- A selection (the primary one; `selections` says how many there are): from and to like the
  cursor, its length in code points, and its text, cut to 200 code points with `…`.
- The viewport: the first and the last visible line, and the headings that start in it, at most
  10.
- `approximate: true` if the server did not have the revision of the indexes yet and applied
  them to the current text. A connection whose client sends no indexes (an older client) has
  only name, device and `updated`, and `position: "unknown"`.

`read_presence` takes `document` and answers JSON with `path`, `revision` and `viewers`, the
list above. `read_document` adds one line per entry after the header, for example:

```
- alice (Pixel): line 12 col 4 in "## Setup", selected "Some text" (10 chars), viewing lines 1-40 (## Intro, ## Setup), 12 s ago
```

Both are read tools, also for app tokens with the scope `read` and for session tokens. They do
not load a page nobody has open, and `read_presence` does not mark the page as seen for
`changes_since`. The write answers and `point_at` keep the names only (`viewers`).

**`point_at`** takes `document` and exactly one of:

| Parameter | |
|---|---|
| `select` | a text of the page that occurs exactly once (like `old` of `edit_document`): the selection |
| `heading` | the text of a heading: the cursor goes to its start and the viewport over its section |
| `clear` | true: remove the focus; the agent's presence ends at once |

- `select` and `heading` are at most 4 KiB.
- A heading is an ATX heading (`## Text`) outside fenced code and front matter, like the
  sections of [sync-protocol.md](sync-protocol.md) section 11. Leading `#` and surrounding
  spaces are ignored, the comparison is case sensitive, and a text that is the text of more
  than one heading is an error. The section ends before the next heading of the same or a
  higher level.
- If nobody has the page open, the answer is "nobody has this page open" and nothing happens;
  the page is not loaded for it.
- It does not mark the page as seen for `changes_since`, and app tokens with the scope `read`
  may use it: a reader can already send cursors over the websocket, and the focus names only
  text the token can read.
- Its own rate limit: a burst of 10, then 2 per second, per app token. It does not count
  against the limit of content changes.

## 5. Assistants that cannot use the wiki

Two ways to reach the wiki from assistants beyond Claude Code were looked at in November 2026
and dropped:

- **Connectors of the Claude apps** (claude.ai, desktop, mobile): Anthropic's servers call the
  MCP server, not the user's device, so `/api/mcp` and the OAuth endpoints would have to be
  reachable from the internet. The wiki stays on the home network.
- **Android AppFunctions** (`androidx.appfunctions`, Android 16+): the Android app would
  expose search, read and edit as functions for Gemini and other system assistants, running
  in the app with its own login, so the wiki could stay private. Dropped because Gemini only
  calls the functions of apps distributed through the Play Store, and the app is not
  published there. The Claude app could not call them either: only system assistants get
  `EXECUTE_APP_FUNCTIONS`.
