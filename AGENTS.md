# AGENTS.md

Notes for AI coding agents working on this repository. Human-oriented docs live in `README.md`;
the account system is specified in `docs/auth.md`, the sync protocol in `docs/sync-protocol.md`,
the dashboard in `docs/dashboard.md`, push notifications in `docs/push.md`, the content edit API and search in `docs/content-api.md`, the MCP server in `docs/mcp.md`, OAuth for MCP clients in `docs/oauth.md`, page metadata and the wiki config in `docs/page-meta.md`, QR codes in `docs/qr.md`.

## Package layout

Dependencies point one way, top to bottom:

| Package | Contents |
|---|---|
| `cmd` | Command line, wires everything together |
| `internal/api` | REST API, websocket endpoints, auth middleware, end-to-end tests |
| `internal/collab` | Automerge sync, presence, document state, authorship, the move engine |
| `internal/gitstore` | Git repository access, file history, the committer |
| `internal/push` | Web Push: VAPID, encryption, who gets a push |
| `internal/activity` | Pins, the change feed, push subscriptions and QR code scans (`activity.db`) |
| `internal/references` | Link parsing, the reference index, text edits for moves |
| `internal/wiki` | The tree of sections, documents and resources, page metadata, QR code keys, the file watcher |
| `internal/auth` | Accounts, sessions, passkeys, audit log |
| `internal/metrics` | Prometheus metrics and their server |
| `internal/nativemem` | Limits and releases the malloc memory of Automerge (glibc only) |
| `internal/configuration` | Configuration file |

automerge-go comes from `third_party/automerge-go` (a `replace` in `go.mod`): a copy whose prebuilt
libraries have their LLVM bitcode stripped, because newer Go releases refuse it. Do not upgrade
Automerge: the backend stays on core 0.5.0 on purpose, Automerge 3 was a lot slower. Details in
`third_party/automerge-go/README.md`.

`collab`, `gitstore` and `references` do not know about HTTP or echo. They reach each other through
small interfaces (`references.LiveTexts`, `gitstore.DocumentSync`), so a new dependency that points
upward means the code is in the wrong package.

## Passkeys in Markus' deployment

The family wiki runs this backend with the passkey RP ID `markusressel.de`, not a wiki host:
the Android app may only use passkeys when Google's Digital Asset Links service can fetch
`https://<rpId>/.well-known/assetlinks.json` from the internet, and the wiki hosts are only
reachable from Markus' home network. The file is in the repository `markusressel/blog`
(`public/.well-known/assetlinks.json`), details in `docs/auth.md` section 12.

- Never change the RP ID of a running deployment: every existing passkey stops working.
- A new Android package name or signing key needs an entry in `server.auth.webauthn.android`
  of the deployment config, in the blog's `assetlinks.json`, and in the web client's
  `MKDOCSEDITOR_ASSETLINKS_JSON`. Tell Markus; the deployment config is in his infrastructure
  repository, not here.
