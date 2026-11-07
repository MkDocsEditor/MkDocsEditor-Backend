# Push notifications

The backend sends a push message when something changes in the wiki that an account follows.
It speaks standard Web Push, so it serves UnifiedPush on Android (with a distributor such as
ntfy, no Firebase) and could serve browsers too:

- [RFC 8030](https://www.rfc-editor.org/rfc/rfc8030) to deliver the message,
- [RFC 8291](https://www.rfc-editor.org/rfc/rfc8291) to encrypt it (`aes128gcm`), so the push
  service cannot read it,
- [RFC 8292](https://www.rfc-editor.org/rfc/rfc8292) (VAPID) to sign it.

A push carries no page content. It tells the client to look at the
[feed](dashboard.md#feed), which the client then fetches itself.

## Configuration

```yaml
server:
  push:
    # The push services the server sends to, as host or host:port. Push is off without any.
    allowedHosts:
      - "ntfy.example.com"
    # Contact in the VAPID tokens. Default: backendUrl, else wikiUrl
    subject: "mailto:admin@example.com"
    # Turns push off
    disabled: false
```

The endpoints are URLs that clients hand to the server. The server only sends to `https`
endpoints on these hosts, so an account cannot make it call other addresses. It does not
follow redirects. A host without a port allows the default port 443 only.

The VAPID key is created on the first start in `<stateDir>/push-vapid.key`. Keep it with
the other state: with a new key, the push services refuse the existing subscriptions until
the clients subscribe again. The subscriptions live in `activity.db`.

Push needs accounts: with `server.auth.disabled` nobody can subscribe.

## Discovery

`GET /server-info/` lists `push` in `features` when push is on, and has the VAPID public key,
the uncompressed P-256 point in base64url without padding:

```json
{ "features": ["…", "push"], "push": { "vapidPublicKey": "BP4z9KsN6nGRTbVYI_c7…" } }
```

When push is off, `push` is `null` and the endpoints below answer 404 `not-found`.

## Subscriptions

The endpoints need a session (cookie or bearer token). App tokens get 403. Every role may
subscribe, readers too.

| Request | |
|---|---|
| `GET /push/subscriptions/` | the subscriptions of the own account |
| `POST /push/subscriptions/` | subscribe |
| `DELETE /push/subscriptions/` | unsubscribe, with `{"endpoint": "…"}` |

The body of `POST` is the format of a browser `PushSubscription` and of UnifiedPush 2.x:

```json
{ "endpoint": "https://ntfy.example.com/upAbC123", "keys": { "p256dh": "BCVxsr7N…", "auth": "BTBZMqHH6r4Tts7J_aSIgg" } }
```

All three answer the list of the own subscriptions, newest first:

```json
{ "subscriptions": [ { "endpoint": "https://ntfy.example.com/upAbC123", "createdAt": "…", "lastPushAt": null, "current": true } ] }
```

`current` marks the subscription of the requesting session. `lastPushAt` is the last push
the push service accepted.

- `POST` is idempotent. Posting an endpoint again replaces its keys and binds it to the
  current session and account, also when it belonged to another one.
- `DELETE` of an endpoint that is not subscribed changes nothing.
- An account has at most 10 subscriptions. A new one replaces the oldest.
- A subscription belongs to the session that made it. When the session ends (logout,
  revoke, expiry), the subscription is deleted at the next push. After a new login the
  client subscribes again.
- An endpoint that answers 404 or 410 is deleted.

Errors of `POST`:

| Status | Code | |
|---|---|---|
| 400 | `bad-request` | the keys are no P-256 point and 16 byte secret |
| 400 | `endpoint-not-allowed` | not https on an allowed host; `details.allowedHosts` lists them |
| 404 | `not-found` | push is off |

## Who gets a push

A push belongs to a feed entry. An account gets the push of an `edited`, `moved` or
`deleted` entry of a document when both hold:

- At least one author of the entry is another account. Changes of the server alone
  (`External change`, `Server`) are no reason.
- The account follows the document: it has a family pin, a personal pin of that account,
  or an `edited` entry of that account in the feed (365 days). For a move both the old and
  the new path count. A delete removes the pins first, so a deleted document reaches only
  those who edited it.

`created` entries and sections are never pushed. Disabled and deleted accounts get nothing.

The push of an entry goes out 1 minute after its last change. While an editing session
goes on, the minute starts again, and an account gets at most one push per entry, on all its
devices. An entry that is removed again (the edit was undone) is not pushed. The pending
pushes live in memory: a restart in that minute loses them. Clients therefore also check the
feed on their own, for example when they start.

The push decides when to look, not what to show. Clients apply their own rules to the
feed: the Android app, for example, does not notify for entries whose only author is
`External change`. So a push may lead to a fetch without a notification.

## The message

The decrypted payload:

```json
{ "type": "feed-changed", "entryId": "42" }
```

`entryId` is the `id` of the entry in `GET /feed/`. Clients ignore payload fields and types
they do not know.

Headers:

| Header | Value |
|---|---|
| `TTL` | `14400` (4 hours) |
| `Urgency` | `normal` |
| `Topic` | `feed`: a newer push replaces one the push service did not deliver yet |
| `Content-Encoding` | `aes128gcm` |
| `Authorization` | `vapid t=<JWT>, k=<public key>` |

The server does not retry. Other answers than 2xx, 404 and 410 are logged and dropped.
