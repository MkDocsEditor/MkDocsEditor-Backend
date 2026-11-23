# Followable agents: agent focus, live edits and guiding

Status: draft, nothing of this is implemented. When it is, the protocol parts move to
[sync-protocol.md](sync-protocol.md) section 6 (Agents) and the tools to [mcp.md](mcp.md).

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
- **The agent's position is sent as an index plus a revision**, not as an Automerge cursor.
  The server knows the index of what it just changed or selected, and cannot make a cursor.
  The client turns it into a cursor of its own (section 3).
- **Hints, not state.** Everything here is presence: ephemeral and never stored. A client that
  ignores it sees the same document as before.

## 3. Protocol

### Agent focus

The `agent` object of presence (and of the `peers` in `presence-snapshot`) can have `focus`:

```json
{ "type": "presence", "documentId": "…", "clientId": "agent-3f9a1c0b7d2e",
  "agent": { "tokenId": "…", "name": "claude",
             "focus": { "revision": "<heads>", "from": 120, "to": 168,
                        "viewport": { "from": 100, "to": 900 } } },
  "selections": [] }
```

- `revision` is the revision (heads, the format of [content-api.md](content-api.md)) the
  indexes refer to.
- `from` and `to` are the selection, `from == to` a cursor. Indexes count **Unicode code
  points** in `content`, starting at 0, `to` after the last selected character. Code points
  because UTF-16 and UTF-8 offsets differ between the clients, which is why people's presence
  uses Automerge cursors.
- `viewport` is optional: the part of the text the agent wants to show, same meaning as the
  viewport of people. A follower centers its middle, and without one it scrolls to the cursor.
- To use the focus, a client creates a cursor at `from` and at `to` in its own document **at
  `revision`** (`Automerge.getCursor(Automerge.view(doc, heads), …)` in JS), and resolves
  it in the current document. That keeps the position correct when people edit after the
  agent set it. If the client does not have `revision` yet, it waits for the next sync. If it
  cannot do this at all, it uses the indexes on the current text.
- `selections` stays `[]`: clients that do not know `focus` see an agent without a cursor, as now.
- A focus ends with the next presence of the agent without one, or with `presence-leave`.
  `presence-snapshot` includes the current focus, so a client that joins later can follow.

### Live edits

An agent change that is meant to be followed gets a presence message before its `sync`
message, with `agent.live`:

```json
{ "type": "presence", …, "agent": { "tokenId": "…", "name": "claude",
  "live": { "revision": "<heads after the change>" },
  "focus": { "revision": "<same>", "from": 560, "to": 560 } } }
```

- `live.revision` ties the hint to one change: the client animates the ranges the agent's
  actor inserted between the revision it had and `live.revision`. It finds the agent actor in
  the actor map ([sync-protocol.md](sync-protocol.md) section 2). Anything else in the change shows at once, deleted text
  too.
- `focus` is the end of the inserted text, so a follower can also follow without animating.
- Order: the presence message goes out before the `sync` message that carries the change. The
  backend does this already for agent presence.
- Without `live` the client shows an agent change at once. A client may also show a live
  change at once when it is large (suggested threshold: 2 KB inserted), when the document is
  not visible, or when the person turned the animation off. The backend does not know and
  does not care.
- Any input of the person (typing, a click, a scroll away) finishes the animation first: the
  real document already holds the complete text, so nothing is lost and no position changes.
  Only what is drawn changes.

### Lifetime

The presence of an agent ends 60 seconds after its last change (`AgentPresenceLifetime`). A
focus set by `point_at` keeps it for 5 minutes after the last move, so a guided tour can
pause. Both end with `presence-leave` without a ghost.

### Feature flag

`agent-focus` in `features` of `GET /server-info/`. A client that follows an agent checks it.

## 4. MCP tools

### Live parameter

`edit_document` and `write_document` get an optional `live` (boolean, default false). It sets
`agent.live` as above. The change itself is unchanged: atomic, with the new `revision` in the
answer, and the same rate limit.

`create_document` with `content` gets the same parameter.

### `viewers`

So that the agent can choose, `read_document` and the answers of the write tools tell who has
the page open: `viewers`, the display names of the accounts with a connection to it,
without agents and without the agent's own account. The tool descriptions say: without
viewers, change instantly; with viewers and a small or medium change, consider `live`.

### `point_at`

Moves the agent's cursor, selection and viewport and changes no text.

| Parameter | |
|---|---|
| `document` | page path or id, like the other tools |
| `select` | a text of the page that occurs exactly once (like `old` of `edit_document`): the selection |
| `heading` | the text of a heading: the cursor goes to its start and the viewport to the section. An alternative to `select` |
| `clear` | true: remove the focus again |

- Exactly one of `select`, `heading` and `clear`.
- Errors as in `edit_document`: "select occurs more than once, add context", "no such heading".
- The server resolves the text at the current revision and sends it as `focus` with that revision.
- It does not mark the page as seen for `changes_since`.
- Rate limit: its own, 2 per second per app token, not the content write limit, because it
  changes nothing. App tokens with the scope `read` may use it. Reviewers: decide this.

## 5. What each side builds

| Side | Work |
|---|---|
| Backend | `focus` and `live` in presence (`internal/collab/agentpresence.go`), `live` and `viewers` in the tools, `point_at`, the feature flag, the 5 minute focus lifetime, tests with a websocket client |
| Web client | follow an agent with `focus` (like a person), the typing animation, a setting to turn it off |
| Android client | the same |

Order: the backend part first. Following an agent works with `focus` alone, before any client
has the animation. The animation can come later and per client.

## 6. Open questions

- Is a code point index good enough, or do JS and Java make another unit easier? The
  conversion is one loop in each client.
- `Automerge.getCursor` with heads exists in JS. Does automerge-java have an equivalent?
  If not, the clients use the indexes on the current text, and `focus` drifts when people
  edit at the same time.
- Does an animation of replaced text (delete and insert) read well, or should it show the
  insert only?
- Should the person be able to say "never animate" and "always animate agents" in the client
  settings, and does the server need to know? (The draft says no.)
