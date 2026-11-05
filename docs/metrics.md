# Metrics and the sync event log

The backend reports what it is doing in two ways:

- **Prometheus metrics** at `/metrics`: rates, durations and current values.
- **The sync event log**: one JSON line per Automerge event, for a log system such as Loki.

Metrics have no per-document labels, because that would create a time series for every document
that ever existed. The event log has the document path, so per-document questions ("which pages
were edited most") go there.

## Configuration

```yaml
server:
  metrics:
    enabled: true    # serve /metrics
    host: "0.0.0.0"  # default: server.host
    port: 9413       # default
    eventLog: true   # write the sync events to stdout
```

Both are off by default. The backend refuses to start if the metrics port cannot be opened.

`/metrics` has its own port and no authentication, and it contains usernames. Keep the port
inside your network: do not route it through the reverse proxy that publishes the API. The API
port does not serve `/metrics`.

The event log contains usernames and document paths, as the normal log does. It never contains
document text.

## Metrics

All metrics start with `mkdocseditor_`. The Go runtime (`go_*`) and process (`process_*`) metrics
are exposed too.

| Metric | Labels | Meaning |
|---|---|---|
| `build_info` | `version`, `commit` | Always 1 |
| `users_online` | | Distinct users with a document open |
| `document_connections` | `access` (`write`, `read`) | Open document websockets |
| `event_connections` | | Open events websockets |
| `documents_loaded` | | Documents in memory |
| `documents_dirty` | | Loaded documents with changes not on disk yet |
| `tree_items` | `type` (`section`, `document`, `resource`) | Items in the wiki |
| `git_pending_documents` | | Documents with uncommitted changes (only with git enabled) |
| `git_pending_changes` | | Uncommitted Automerge changes (only with git enabled) |
| `sync_messages_total` | `direction` (`received`, `sent`) | Automerge sync messages |
| `sync_bytes_total` | `direction` | Bytes of sync messages, before base64 |
| `sync_changes_total` | `user` | Automerge changes in messages that changed a document |
| `sync_apply_duration_seconds` | | Time to apply a sync message of a client |
| `sync_rejected_total` | `reason` | Messages not applied: `invalid-message`, `apply-failed`, `foreign-actor`, `read-only` |
| `presence_messages_total` | | Cursor and selection messages from clients |
| `external_changes_total` | `result` | File changes on disk merged into a document |
| `document_loads_total` | `result` | Documents loaded into memory |
| `document_load_duration_seconds` | | Time to load a document and merge its file |
| `document_state_corrupt_total` | | Saved states that could not be loaded (a new epoch started) |
| `document_flushes_total` | `result` | Writes to the file and the state directory, retried on error |
| `document_flush_duration_seconds` | | Duration of those writes |
| `document_state_size_bytes` | | Size of the saved Automerge state at each flush |
| `tree_rebuilds_total` | | Rebuilds of the item tree |
| `websocket_connections_total` | `type` (`document`, `events`) | Accepted websocket upgrades |
| `websocket_send_queue_full_total` | | Clients disconnected for not keeping up |
| `websocket_heartbeat_timeouts_total` | | Clients disconnected for missing heartbeats |
| `git_commits_total` | `result` (`ok`, `empty`, `error`) | Commit attempts |
| `git_commit_duration_seconds` | | Time to create a commit |
| `git_pushes_total` | `result` | Pushes |
| `auth_events_total` | `type` | Audit log events, see [`auth.md`](auth.md) |
| `http_requests_total` | `method`, `route`, `status` | Finished requests |
| `http_request_duration_seconds` | `method`, `route` | Duration of requests, without websockets |

`result` is `ok` or `error` unless noted otherwise.

`route` is the route pattern, for example `/document/:id/`. A request that matches no route has
the route `unmatched`, and an unknown HTTP method is `other`, so clients cannot create new time
series. A websocket counts in `http_requests_total` when it closes, with status 101 if the upgrade
succeeded.

`GET /alive/` is not in the HTTP metrics and not in the request log, because a readiness probe calls
it every few seconds.

## Sync event log

With `eventLog: true`, each event is one JSON line on stdout. The normal log goes to stderr.

```json
{"time":"2026-11-05T10:12:01.52Z","type":"sync-event","kind":"changes-applied","path":"guide/setup.md","user":"alice","clientId":"c-7","actor":"8f3a...","changes":3,"bytes":412,"heads":1,"durationMs":0.84}
```

| `kind` | When |
|---|---|
| `client-joined`, `client-left` | A client opened or closed a document. Has `client`, `access`, `connections` |
| `changes-applied` | A sync message changed a document. Has `changes`, `bytes`, `actor`, `heads` |
| `sync-rejected` | A sync message was not applied. Has `reason`, the same values as `sync_rejected_total` |
| `external-change` | A file change on disk was merged. Has `reason` on failure |
| `flushed`, `flush-failed` | The document was written to its file and the state directory |
| `document-loaded`, `document-load-failed`, `document-unloaded`, `document-deleted` | The document entered or left memory |
| `state-corrupt` | The saved state could not be loaded, the document starts a new epoch |

`heads` above 1 means there are concurrent edits that no later change has merged yet. Sync messages
that change nothing and presence messages are only counted, not logged.

A LogQL query for Grafana:

```
{container="mkdocseditor-backend"} |= `"type":"sync-event"` | json | kind="changes-applied"
```
