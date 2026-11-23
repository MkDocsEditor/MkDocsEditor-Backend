# Followable agents: agent focus, live edits and guiding

Status: the agent focus, its lifetime, `point_at`, `viewers` and the feature flag
`agent-focus` are implemented and documented in [sync-protocol.md](sync-protocol.md) section 6
(Agents) and [mcp.md](mcp.md) section 4, which are authoritative for them. Live edits
(`agent-change`, `live`) and the focus that a change sets are not implemented yet.
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
  there. automerge-java 0.0.9 counts code points as well (tested: `"a😀b"` has length 3), so
  the Android client uses the indexes as they are with its Automerge calls and converts only
  for the editor (Kotlin strings are UTF-16). JS counts UTF-16 code units and converts on the
  text of `Automerge.view(doc, heads)`.

## 3. Protocol

### Revisions

All revisions are in the format of [content-api.md](content-api.md) (the heads). The revision of
a **change** is the heads of the change itself, `[H]`, not of the merged document: an agent edit with
an old `baseRevision` is made on a fork of that revision and merged afterwards, so its indexes
are only right at the heads of the fork after the commit, which are the hash `H` of the agent's
change. The server must not use the heads of the merged document. The revision of `point_at`
has no change: it is the revision the text was read at, which can be several heads.

One limit applies everywhere a client waits for a revision or a change it does not have yet:
**30 seconds**, called the wait limit below.

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
  character, so it can be the length of the text (exclusive).
- `viewport` is optional and has the meaning of the viewport of people: `top` is the index of
  the first and `bottom` the index of the last visible character (both inclusive, unlike `to`).
  A follower centers their middle, and without a viewport it scrolls to the cursor. For an
  empty text both are 0.
- `selections` stays `[]`: clients that do not know `focus` see an agent without a cursor.
  Both clients ignore unknown keys and unknown message types today (web: the `default` of
  `handleMessage`; Android: Moshi codegen adapters without `failOnUnknown`, and
  `MessageEnvelope.type` is a string). The Android parser does fail when a known field changes
  its type or becomes null, so existing fields of presence keep their types.
- To use a focus, a client creates a cursor for each index in its own document at `revision`
  (JS: `Automerge.getCursor` on `Automerge.view(doc, heads)`; Java: `makeCursor` with heads) and
  resolves it in the current document.
  - First it checks that its document has every head of `revision` (JS: `Automerge.hasHeads`).
    JS `Automerge.view` at heads the document does not have does not fail: its `content` is
    undefined and `getCursor` on it silently returns the end of the text.
  - An index at the end of the text at `revision`, in both clients: the client makes the
    cursor for `index - 1` and adds 1 after resolving. automerge-java throws for
    `makeCursor(length)`, also with heads ("index 3 is out of bounds"), and in JS
    `getCursor(view, length)` returns the end-of-text cursor `"e"`, which resolves to the end of the *current*
    text and would jump when people append. If the text at `revision` is empty, the position
    is `"start"`.
  - A cursor whose character was deleted since resolves to where the text was: the index of
    the next surviving character. Both clients do this without an error (tested: JS
    `getCursor` with its default `move: "after"`, Java 0.0.9 `lookupCursorIndex`). JS clients
    use the default and do not pass `move: "before"`. Should a cursor still not resolve, the
    client applies `from` and `to` to the current text, clamped to its length. It never moves
    to the start of the text.
  - A client that does not have `revision` yet waits for the sync. After the wait limit it
    stops waiting and applies `from` and `to` to the current text, clamped to its length.
- The focus is part of the presence of the agent, so `presence-snapshot` contains it and a
  client that joins later can follow.
- The server never puts the focus of an agent into `lastPositions` or ghosts. They are
  per account (`rememberPositionLocked`), and would overwrite the last position of the person
  whose account the agent uses.
- A new epoch only starts when a session loads from lost state, so the server has no agent
  presence to clear then. A client that sees a new epoch drops its queued `agent-change`
  events and every agent focus it has.

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
  message can be early), and drops it if the change does not arrive within the wait limit.
  `presence-snapshot` and later joins never replay it. On `document-renamed` the queue stays,
  because the hashes do not change.
- When the client has `H`, it computes the diff from `base` to `[H]`. The splices that insert
  text are what the agent typed (JS: patches with action `splice`; Java: `SpliceText`). The
  client animates those and shows everything else at once, deleted text too (JS: `del`; Java:
  `DeleteList`). A replacement comes as an insert and a delete.
- The patches of a diff are sequential: each index refers to the text after the patches
  before it. They are not sorted by position, and the order differs between the libraries
  (tested: JS in the order of the operations, Java 0.0.9 in document order, with neighbours
  merged). The client therefore maps them to positions in the text at `[H]` first, then makes
  cursors for each inserted range at `[H]` and resolves them in the current document, so
  ranges stay right when people edited meanwhile.
- One editor transaction of a client can hold the agent's change together with changes of
  people that arrived in the same sync. The client never takes the inserted ranges from that
  transaction; they come only from the diff and the cursors at `[H]`.
- The same change also sets the `focus` of the agent: the end of the last inserted range in
  the order of the edits (not the highest position), at revision `[H]`. It is sent in the
  presence message of the agent, which goes out before the `agent-change` message.
- While a client reveals a change, it draws the agent's cursor at the head of the reveal,
  and a follower follows that head. It switches to `focus` when the reveal ends or is
  finished early. Otherwise a follower would jump to the end while the text is still being
  typed.
- A client may show a live change at once when it is large (suggested threshold: 2 KB
  inserted), when the document is not visible, when the setting "Animate edits of others" is
  off, or with reduce motion (see "Client animation").
  The backend does not know and does not care.
- Without an `agent-change` message the client shows an agent change at once, and the focus
  goes to the end of the last inserted range as well (the same rule, section 3 "Lifetime").
- How the clients draw it, the same on both, is in "Client animation" below.

### Client animation

Both clients behave the same. This is client behaviour, not protocol: the backend is not
involved. It has two parts:

- The **typing reveal** shows the inserted text of a live agent change (an `agent-change`
  event) bit by bit, as if it were typed.
- The **appearance animation** fades in text that others write as it appears: what the
  typing reveal uncovers, and the text of other people in the document as it arrives.

Rules:

- The app setting **"Animate edits of others"** turns the appearance animation on and off. It
  is **on by default** in the web and Android clients (the default of the KodeEditor
  configuration stays off). It is only for text that others write (an agent, or another person
  in the document), never for text the local person types, pastes, undoes or redoes. With the
  setting off, a live agent change shows at once too, without a reveal.
- Defaults, all settable by the client:
  - fade-in 250 ms
  - an optional slide of the appearing characters, 0 by default. It is optional per client;
    the web client leaves it out for now.
  - only the newest 16 characters of one writer animate
  - at most 24 animated ranges at once; above that, the oldest finish at once
- Every animated range has a tag of its writer: the actor id of the change. The clients
  already know which account, and for agents which app token, an actor belongs to (the actor
  map, [sync-protocol.md](sync-protocol.md) section 2), so this needs no message. Several
  writers can appear at the same time. A new change of the same agent finishes that agent's
  running reveal at once, then starts its own.
- Input of the person never ends a fade, neither of an agent's text nor of other people's.
  Only the agent's reveal can be skipped, in two ways:
  - A tap or click on the area where text is being revealed right now skips the reveal: the
    rest of the agent's text shows at once. Typing elsewhere, moving the cursor elsewhere and
    scrolling do not.
  - An edit of the person, or a remote edit, inside or at the edge of a hidden range finishes
    the reveal of that range, so nobody types into invisible text.
- Collapsed or folded parts of the text appear in one chunk when the reveal reaches them. They
  are never typed out, and never shown before the reveal reaches them. (The web editor has no
  folding today.)
- With the system setting that removes animations (reduce motion; web:
  `prefers-reduced-motion: reduce`), a live agent change shows at once: no reveal, no fade, no
  slide. A follower still follows the agent's focus.
- The cost per frame depends on the visible rows and the number of animated ranges, never on
  the length of the document.
- The animation is presentation only. The change is applied to the editor model at once, so
  that remote edits and the cursor of the person land at the right positions; only what is
  drawn changes.
- The hidden text keeps its space on both clients: it is drawn with a transparent text colour,
  so the following text stays where it is and does not slide along as the agent types
  (decided by Markus). Nothing moves under the eyes of a person who reads, and both clients
  look the same.
- The hiding starts in the same editor transaction that applies `H`. Added in a later
  transaction, the whole text would show for a frame first. So a client must know which
  applied changes contain `H`; that is client work, not protocol. The decoration only maps
  through later transactions and never holds a change back.
  - Web: a CodeMirror 6 `Decoration.mark` with a transparent text colour, held in a
    `StateField` and mapped through every transaction. Not `Decoration.replace`, which would
    collapse the text.
  - Android: `EditorTextController.onRemoteChange` calls `applyRemote` synchronously on the
    main thread; the hidden ranges are set in the same call and moved per splice like the
    author runs. Local edits are reported synchronously in `onLocalEdit` and
    `onStateChanged`, where the client finishes what the rules above say. The text is
    hidden and animated with the KodeEditor API for styles of app-supplied ranges
    (`SpanStyle(color = Transparent)`), merged over the highlighting and moved by splices
    (KodeEditor PR #167). KodeEditor offsets are UTF-16, so the client converts the code
    point indexes of this spec. Overlay masks in the background colour are not used: they
    would also cover the selection, remote cursors and line backgrounds, and could be drawn
    one frame late with the old layout. The Android animation needs a KodeEditor release
    with that API; before it, Android follows the focus without an animation. Nothing else
    in this spec depends on that API.
- Search, copy, undo and the rendered view see the whole text at once, because it is real.
  Animations stay short for that reason.
- The rendered view (alone, or beside the editor) shows a live change at once and is never
  animated.
  - Web: a follower in the rendered view scrolls to the line of the focus, because the
    rendered view can scroll to a source line but not to a character.
  - Android never follows in a rendered-only view: following opens the read-only editor
    (`startFollowing`), and leaving the editor stops following. A rendered pane beside the
    editor scrolls along with the editor.

### Lifetime

Every agent change and every `point_at` sets a focus, and an agent presence with a focus lasts
5 minutes after the last of them, because a turn of the model often takes longer than a minute
and followers must not be dropped in the middle of a tour. This is a visible change: the agent
stays in the list of people who are here for 5 minutes after an edit, not 60 seconds. It ends
with `presence-leave` without a ghost. `point_at` with `clear` ends it at once. An instant
edit sets the focus to the end of the last inserted range, or to the place of the change if it
only deleted, so it does not end an existing focus. The old 60 seconds (`AgentPresenceLifetime`)
only apply to a presence that never had a focus. The presence ends when the document unloads:
that can be later than when the last person closes it, because a session with unsaved
changes stays loaded until the flush works.

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
  and `PUT /document/{id}/content/` share the collab code and, with an app token, also set
  the focus (instant), but get no `live`.
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
- A heading is an ATX heading (`## Text`) outside fenced code and front matter, as for the
  section restore; setext headings are not supported. The comparison
  ignores the leading `#` characters and surrounding spaces and is case sensitive. A text that
  occurs as the text of more than one heading is an error.
- If nobody has the page open, the answer is "nobody has this page open" and nothing
  happens: a focus on a page without a connection would be lost anyway.
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
  - `SetAgentFocus` must not use `withSession`, which loads a temporary session for a closed
    page. Under the lock of an existing session it checks that the page has connections
    (else `ErrNobodyOpen`; a separate check followed by a set would be a race), that the
    revision is known (`baseHeadsLocked`), and that `from`, `to`, `top` and `bottom` are not
    beyond the length in code points of the text at that revision.
  - `ApplyEdits` and `WriteContent` get an options parameter (`live`) that REST passes as zero.
    `commitAgentForkLocked` gets it and the inserted ranges. `SpliceDiff` returns only an error
    today (`textdiff.go`), so it has to report its ranges as well, for the focus of
    `write_document`.
- The commit of an agent change (`commitAgentForkLocked`) already sends the presence before the
  sync message through the same queue per connection. The `agent-change` message goes in
  between.

## 6. What each side builds

| Side | Work |
|---|---|
| Backend | `focus`, `agent-change` and the lifetime (`internal/collab/agentpresence.go`), `live` and `viewers` in the tools, `point_at`, the feature flag, tests with a websocket client |
| Web client | follow an agent with `focus` (like a person), the typing reveal and the appearance animation of "Client animation" (section 3) with the setting "Animate edits of others", on by default; no slide for now |
| Android client | the same; the animation with the KodeEditor release that has range styles |
| KodeEditor | an API for styles of app-supplied ranges, moved by splices (transparent text colour, fade-in): PR #167, released in KodeEditor 9.2.0 |

Order: the backend part first. Following an agent works with `focus` alone, before any client
has the animation, and each client can add the animation later. The Android animation depends
on the KodeEditor API; nothing else does.

## 7. Verified and not verified

Tested against the libraries the clients use (`@automerge/automerge` 3.5.0, automerge-java
0.0.9) with throwaway tests:

- Index units: JS UTF-16 code units, automerge-java Unicode code points.
- A cursor made at an older revision resolves right after concurrent inserts (JS, Java).
- A cursor on a character deleted later resolves to the next surviving character and does not
  throw (JS, Java).
- `makeCursor` at the end of the text throws in Java, also with heads; JS `getCursor` at the
  end returns the end-of-text cursor `"e"`. JS `view` at unknown heads does not fail.
- `diff` patches are sequential and unsorted (JS: operation order; Java: document order).
- Unknown keys and message types are ignored by both clients (Android by reading the code and
  a test; web by reading the code).

Still open:

- An emoji (outside the BMP) case in the interop tests of both clients, so the units above
  stay pinned.
- Whether the Automerge sync can hold back a change after the `agent-change` message (the
  queue by `change` is written for it).
- The animation in the web editor is known by reading the code only, without a prototype:
  feasible with a CodeMirror 6 mark decoration and the one-transaction rule. The KodeEditor
  demo of the reveal and the appearance animation was tested by Markus.
