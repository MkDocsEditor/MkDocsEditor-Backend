# Followable agents: agent focus, live edits and guiding

Status: draft, nothing of this is implemented. When it is, the protocol parts move to
[sync-protocol.md](sync-protocol.md) section 6 (Agents) and the tools to [mcp.md](mcp.md).
Revised after a review of the first draft.

## 1. Goal

An agent that edits a page over MCP ([mcp.md](mcp.md)) changes the text in one step, and people
who have the page open see it jump. People should be able to follow an agent like they follow
another person, and an agent should be able to choose how its change shows up:

1. **Instant**: the text changes at once. The default, and the right choice for big or
   mechanical changes and when nobody looks.
2. **Live**: the clients show the inserted text as if it were typed, and a follower sees the
   agent's cursor move with it.
3. **Guiding** (no text change): the agent moves its cursor, selection and viewport to a
   place of a page, so that followers are taken there.

The agent decides per change. Following an agent is already specified for people
([sync-protocol.md](sync-protocol.md) section 6, Viewport) and is turned off for agents in the web and
Android clients today, because agent presence has no cursor (`selections: []`): automerge-go
cannot create Automerge cursors, and the backend stays on Automerge core 0.5.0.

## 2. Decisions

- **The typing animation is a client effect, not a backend job.** The backend still commits a
  live edit as one atomic change and answers with the new revision. A server that committed
  the text piece by piece would write dozens of changes per edit into the document history
  and every sync, would need jobs in memory (lost on a crash, half typed text), could not
  return a revision, and would give every client the same pace. A client can skip it, slow it
  down, or turn it off.
- **A live edit is announced by an event, not by presence state.** Presence means "the latest
  state wins". A change that has to be animated is an event: the client needs every one of
  them, also when two arrive before the next sync.
- **The clients find the inserted text with a diff, not with patches.** Automerge patches do
  not say which actor made them. `Automerge.diff(doc, before, after)` in JS and
  `Document.diff(ChangeHash[], ChangeHash[])` in automerge-java give exactly the splices
  between two sets of heads, in the unit of the client.
- **The agent's position is sent as an index plus a revision**, not as an Automerge cursor.
  The server knows the index of what it just changed or selected, and cannot make a cursor.
  The client turns it into a cursor of its own at that revision (section 3), so the position
  stays correct when people edit afterwards.
- **Hints, not state.** All of it is ephemeral and never stored. A client that ignores it sees
  the same document as before.
- **Indexes count Unicode code points** of `content`, as the backend does everywhere else
  (`placeEdits`, authorship, references, text diff). This changes the rule "indices never
  go over the wire" of sync-protocol.md section 1: it needs a new wording when this moves
  there. The clients convert (JS: the text of `Automerge.view(doc, heads)` is UTF-16).

## 3. Protocol

### Revisions

All revisions are in the format of [content-api.md](content-api.md) (the heads). A revision in
a hint is the heads of **the change itself**, not of the merged document: an agent edit with
an old `baseRevision` is made on a fork of that revision and merged afterwards, so its indexes
are only right at the heads of the fork after the commit, which are the hash `H` of the agent's
change. The server must not use the heads of the merged document.

### Agent focus

The `agent` object of presence (and of the `peers` in `presence-snapshot`) can have `focus`:

```json
{ "type": "presence", "documentId": "…", "clientId": "agent-3f9a1c0b7d2e",
  "agent": { "tokenId": "…", "name": "claude",
             "focus": { "revision": "<heads>", "from": 120, "to": 168,
                        "viewport": { "top": 100, "bottom": 899 } } },
  "selections": [] }
```

- `revision` is the revision the indexes refer to.
- `from` and `to` are the selection, `from == to` a cursor. `to` is after the last selected
  character, so it can be the length of the text.
- `viewport` is optional and has the meaning of the viewport of people: `top` is the index of
  the first and `bottom` the index of the last visible character (both inclusive). A follower
  centers their middle, and without a viewport it scrolls to the cursor.
- `selections` stays `[]`: clients that do not know `focus` see an agent without a cursor.
- To use a focus, a client creates a cursor for each index in its own document at `revision`
  (JS: `Automerge.getCursor` on `Automerge.view(doc, heads)`; Java: `makeCursor` with heads) and
  resolves it in the current document.
  - An index at the end of the text at `revision`: `makeCursor(length)` throws in automerge-java
    (sync-protocol.md section 6). The client makes the cursor for `index - 1` and adds 1 after
    resolving, or uses `"end"` if the text at `revision` is the current text.
  - A cursor that does not resolve because its character was deleted: the client moves to the
    nearest resolvable position and never to the start of the text. If it cannot, it uses the
    indexes on the current text.
  - A client that does not have `revision` yet waits for the sync. If it still has no
    such revision after about 10 seconds, it drops the focus and uses the indexes on the
    current text.
- The focus is part of the presence of the agent, so `presence-snapshot` contains it and a
  client that joins later can follow.
- The server never puts the focus of an agent into `lastPositions` or ghosts. They are
  per account (`rememberPositionLocked`), and would overwrite the last position of the person
  whose account the agent uses.
- After an epoch change, the server clears the focus of all agents.

### Live edits

A change of an agent that is meant to be followed gets an `agent-change` message, sent before
the `sync` message that can carry it:

```json
{ "type": "agent-change", "documentId": "…", "clientId": "agent-3f9a1c0b7d2e",
  "instanceId": "…", "agent": { "tokenId": "…", "name": "claude" },
  "base": "<heads before the change>", "change": "<hash H>" }
```

- `base` is the heads of the fork before the commit (the dependencies of `H`), `change` is `H`.
- The message is an event. The client keeps it in a queue by `change` until its document
  contains that change (the Automerge sync can hold a change back for a round trip, so the
  message can be early), and drops it if the change does not arrive within about 30 seconds.
  `presence-snapshot` and later joins never replay it.
- When the client has `H`, it computes the diff from `base` to `[H]`. The splices that insert
  text are what the agent typed. The client animates those and shows everything else at once,
  deleted text too. For each inserted range it makes cursors at `[H]` and resolves them in the
  current document, so ranges stay right when people edited meanwhile.
- The same change also sets the `focus` of the agent: the end of the last inserted range, at
  revision `[H]`, sent in the presence message of the agent (it goes out before the
  `agent-change` message). A follower follows the focus, whether or not the client animates.
- A client may show a live change at once when it is large (suggested threshold: 2 KB
  inserted), when the document is not visible, or when the person turned the animation off.
  The backend does not know and does not care.
- Without an `agent-change` message the client shows an agent change at once, and the focus
  goes to the end of the last inserted range as well (the same rule, section 3 "Lifetime").
- The animation is a decoration that hides the text not yet typed on top of the real
  document. The change is applied to the editor model at once, so that remote edits and the
  cursor of the person land at the right positions. Any input of the person (typing, a click,
  a scroll away) ends the animation first. Only what is drawn changes.

### Lifetime

An agent presence without a focus ends 60 seconds after the last change
(`AgentPresenceLifetime`), as now. With a focus it lasts 5 minutes after the last change or
`point_at`, because a turn of the model often takes longer than a minute and followers must
not be dropped in the middle of a tour. Both end with `presence-leave` without a ghost. An
instant edit also sets the focus (end of the last inserted range, or of the change if it only
deleted), so it does not end an existing focus.

### Joining by link

The join link of [auth.md](auth.md) (access request and join link) picks the sender's
connection with the newest `lastActiveAt`. Agent presences carry the account's `accountId` and
would win with `point_at`. They are never picked: add this sentence to auth.md when this is
built. The web client (`followTarget.ts`) and the Android client (`SenderPosition.kt`) already
skip agents.

### Feature flag

`agent-focus` in `features` of `GET /server-info/`. A client that follows an agent checks it.

## 4. MCP tools

Everything here is for app tokens, and OAuth grants are app tokens. A session token (`mds_…`)
has no token id, so it has no agent presence: `live` and `point_at` answer "needs an app
token". One app token used by two agent sessions at once shares one presence and one focus.

### Live parameter

`edit_document` and `write_document` get an optional `live` (boolean, default false) that
makes the server send `agent-change`. The change is otherwise unchanged: atomic, the new
`revision` in the answer, the same rate limit.

- It is a parameter of the MCP tools only. The REST endpoints `POST /document/{id}/edits/`
  and `PUT /document/{id}/content/` share the collab code but get no `live`.
- With several edits or a `write_document` that splices in many places, the focus is the
  end of the last inserted range, and a client shows many scattered splices at once.
- `create_document` gets no `live`: nobody has a new page open.

### `viewers`

So that the agent can choose, `read_document` and the answers of the write tools tell who has
the page open: `viewers`, the display names of the accounts that have a connection to it. This
includes the account the agent works for (marked `you`), because following the own agent is
the main case. It excludes agent presences and ghosts. The tool descriptions say: without
viewers, change instantly; with viewers and a small or medium change, consider `live`.

The names go to the MCP client and so to its model provider, also for read scope and OAuth
grants. The account can see the same in the web client, so this is no privilege change, but it
is a new place: add a row to the threat table of auth.md, like "last positions reveal
activity".

### `point_at`

Moves the agent's cursor, selection and viewport and changes no text.

| Parameter | |
|---|---|
| `document` | page path or id, like the other tools |
| `select` | a text of the page that occurs exactly once (like `old` of `edit_document`): the selection |
| `heading` | the text of a heading: the cursor goes to its start and the viewport to its section. An alternative to `select` |
| `clear` | true: remove the focus |

- Exactly one of `select`, `heading` and `clear`. Both texts are at most 4 KiB.
- Errors as in `edit_document`: "select occurs more than once, add context", "no such heading".
- A heading is an ATX (`## Text`) or setext heading outside fenced code. The comparison
  ignores the leading `#` characters and surrounding spaces and is case sensitive. A text that
  occurs as the text of more than one heading is an error.
- If nobody has the page open, the answer is "nobody has this page open" and nothing
  happens: a focus on a page without a connection would be lost anyway. When the last person
  closes the page, the focus ends with it.
- It does not mark the page as seen for `changes_since`.
- App tokens with the scope `read` may use it. A reader can already send cursors and viewports
  over the websocket, the focus names only text the token can read, and followers move only
  when they chose to follow.
- Rate limit: its own, 2 per second with a small burst per app token, not the content write
  limit, because it changes nothing.

## 5. Code layout

The dependencies keep pointing downward ([AGENTS.md](../AGENTS.md)):

- `internal/api` resolves `select` and `heading` on the text and revision that `ReadContent`
  returns together, and resolves names for `viewers`. It knows nothing about presence.
- `internal/collab` gets `SetAgentFocus(d, agent, revision, from, to, viewport)`, sends
  `agent-change` from the commit of an agent change, and returns the account ids of the
  connections of a page for `viewers`. It knows nothing about MCP or headings.
- The commit of an agent change (`commitAgentForkLocked`) already sends the presence before the
  sync message through the same queue per connection. The `agent-change` message goes in
  between.

## 6. What each side builds

| Side | Work |
|---|---|
| Backend | `focus`, `agent-change` and the lifetime (`internal/collab/agentpresence.go`), `live` and `viewers` in the tools, `point_at`, the feature flag, the clearing of focus on an epoch change, tests with a websocket client |
| Web client | follow an agent with `focus` (like a person), the diff based animation, a setting to turn it off |
| Android client | the same |

Order: the backend part first. Following an agent works with `focus` alone, before any client
has the animation, and each client can add the animation later.

## 7. Not verified yet

These need a check in the clients before they rely on them:

- Which unit automerge-java 0.0.9 uses for text indexes. Add an emoji (outside the BMP) case to
  the interop tests of both clients.
- What `lookupCursorIndex` (Java) and `getCursor` (JS) do for a character that was deleted
  after the revision. The rule above is the intent, not a result.
- Whether the Automerge sync can hold back a change after the `agent-change` message (the
  queue by `change` is written for it).
- That the animation as a decoration can be done in the editor of each client without delaying
  the application of patches.
