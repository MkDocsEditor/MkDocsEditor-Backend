# QR codes

Stickers with a QR code or an NFC tag on things in the house (the washing machine, the heating)
open a wiki page. The code holds a key, and the page that carries the key in its front matter is
the one that opens. The key lives in the file, so it moves with the page when the page is
renamed or moved; a redirect from the old path would lose to a new page created there.

`GET /server-info/` lists `qr-codes` in `features` on a server with this part. The REST details
are in the [OpenAPI file](../openapi-description.yaml).

## Keys in the front matter

```markdown
---
qr: waschmaschine
---
```

`qr` is one key, a list of keys, or entries with a label, also mixed:

```yaml
qr: [waschmaschine, trockner]
qr:
  - key: waschmaschine
    label: "Keller, an der Maschine"
  - trockner
```

- A key is trimmed and compared in lower case. After that it must match
  `[a-z0-9][a-z0-9-]{0,63}`. Numbers count as text: `qr: 42` is the key `42`.
- A key given twice on one page counts once.
- Other values are invalid. They do not open the page, and `GET /qr/` lists them.
- Only documents carry keys, `index.md` pages included. The front matter rules are those of
  [page-meta.md](page-meta.md).

The tree has the valid keys of a document in `meta.qr` as `[{key, label}]`, `label` being `null`
without one. Clients use it to warn before a page with codes is deleted or its codes are
removed. There is no own event: a change of the front matter rebuilds the tree, and the events
websocket sends `tree-changed`.

## Sticker URL

The sticker holds a URL of the web client, `https://<web host>/q/<key>`. The web client asks
`GET /api/qr/<key>/?scan=1` and opens the page. Stickers hold the host, so a printed sticker
breaks when the web client moves to another host. The backend does not know the host.

## Opening a code

`GET /qr/{key}/` is open to every role. The key is compared in lower case.

| Answer | When | Body |
|---|---|---|
| 200 | one document has the key | `{key, label, item, title, lastScannedAt}` |
| 409 `qr-conflict` | more than one document has it | error with `details: {key, items: [{item, title}], lastScannedAt}` |
| 410 `qr-gone` | no document has it now, but one had it | error with `details: {key, label, lastPath, lastScannedAt}` |
| 404 `not-found` | no document ever had it, or the key is invalid | error |

`item` is an `ItemRef` (`type`, `id`, `path`, `name`) and `title` the page title. 410 means the
page was deleted or its code removed; the web client offers to put the key on a page.

`?scan=1` records the scan time as `lastScannedAt`; without it the request is a preview and
counts nothing. Scans are recorded for keys the backend knows (200, 409 and 410), not for 404,
so unknown keys do not fill the database.

## The list

`GET /qr/` is open to every role:

```json
{
  "codes": [{ "key": "waschmaschine", "label": "Keller", "item": { "type": "document", "id": "…", "path": "haus/waschmaschine.md", "name": "waschmaschine" }, "title": "Waschmaschine", "lastScannedAt": "2026-11-07T10:00:00Z" }],
  "conflicts": [{ "key": "trockner", "items": [{ "item": { … }, "title": "…" }, { "item": { … }, "title": "…" }], "lastScannedAt": null }],
  "invalid": [{ "value": "-bad", "item": { … }, "title": "…" }],
  "gone": [{ "key": "heizung", "label": null, "lastPath": "haus/heizung.md", "lastScannedAt": null }]
}
```

`codes`, `conflicts` and `gone` are sorted by key, `invalid` by path. Items of a conflict are
sorted by path.

## Storage

`activity.db` (the state directory, [dashboard.md](dashboard.md)) has a table `qr_codes`: per
key the last document path, its label and the last scan. Every tree change updates the keys
whose document or label changed; a conflicting key keeps the first document by path. Rows are
never removed, so a gone key keeps answering 410.
