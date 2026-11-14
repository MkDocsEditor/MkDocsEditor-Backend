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
| `list_tree` | | Paths and titles of all pages, or of those below `section` |
| `read_document` | | The markdown of a page with a header: `path`, `title`, `revision` |
| `get_backlinks` | | Pages that link to a page, with line and link text |
| `edit_document` | yes | Replaces text: each `old` must occur exactly once. Optional `baseRevision`. Like `POST /document/{id}/edits/` |
| `write_document` | yes | Replaces the whole text against a required `baseRevision`, merged three-way. Like `PUT /document/{id}/content/` |
| `create_document` | yes | Creates a page in `section` with the file name `name` (without `.md`), optionally with `content` |
| `move_document` | yes | Moves and/or renames a page. Links in other pages are updated, like `POST /move/` |

Errors are tool results with `isError` and a message the agent can act on, for example
"edit 0: old occurs more than once in the text, add context to make it unique".

Content changes share the rate limit of the content API: a burst of 60, then one per
second, per app token. Create and move count too.

## 4. Agents in the editor

An agent that changes a page shows up for the people who have it open:

- **Presence**: a `presence` message as from any peer, with the account's name, colour and
  avatar, plus:
  - `"agent": {"tokenId": "…", "name": "<token name>"}`
  - `deviceName` set to the token name
  - `selections: []`: automerge-go cannot create Automerge cursors, so an agent has no cursor
  - `clientId` `agent-<12 hex>` and a stable `instanceId` derived from the token

  The agent is also part of the `peers` of `presence-snapshot` for clients that connect
  later. A `presence-leave` follows 60 seconds after its last change.
- **Authorship and history**: the agent's actor has `agent` in the actor map. Commits carry
  a `Via-App-Token` trailer, and history entries list the tokens in `appTokens`. See
  [content-api.md](content-api.md) section 5.

Changes over a session token (`mds_…`) are no agent changes: they appear as the account's
own, without presence.
