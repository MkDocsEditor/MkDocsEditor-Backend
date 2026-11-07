# Page metadata and the wiki config

Clients show page names, descriptions and tags in lists and search, and render pages like
MkDocs and Material (or Zensical) do. The backend reads both from the wiki, so clients do
not have to load every page or parse `mkdocs.yml` themselves.

`GET /server-info/` lists `page-meta` and `wiki-config` in `features` on a server with these
parts. The REST details are in the [OpenAPI file](../openapi-description.yaml).

## Page metadata

Every document of the tree (`GET /section/`, `GET /document/{id}/`) has `title` and `meta`:

```json
{
  "type": "document",
  "name": "setup",
  "path": "guide/setup.md",
  "title": "Getting started",
  "meta": {
    "title": "Getting started",
    "description": "First steps",
    "icon": "material/rocket",
    "status": "new",
    "tags": ["setup", "intro"],
    "searchExclude": false
  }
}
```

`meta` holds what the front matter says, nothing else: `title`, `description`, `icon` and
`status` are `null` when it does not set them, `tags` is `[]`, and `searchExclude` is
`search: {exclude: true}`. A single string in `tags` is one tag, and other values are turned
into strings. `name` stays the file name without `.md`.

### Front matter

Front matter is YAML between a first line `---` and a closing line `---` or `...`, as in
MkDocs:

```markdown
---
title: Getting started
tags: [setup, intro]
---
# Setup
```

YAML that cannot be parsed counts as no front matter. The MultiMarkdown form of the `meta`
extension (`Key: value` lines without `---`) is not read.

### Title

`title` is the page name as MkDocs shows it:

1. the front matter `title`,
2. else the first level-1 heading outside fenced code: `# Text` or a line followed by `===`,
   taken as written without closing `#`,
3. else the file name. `-` and `_` become spaces, and a name in lower case gets a capital
   first letter. `index.md` and `README.md` take the name of their folder, and the index of
   the docs folder is `Home`.

Pins and feed entries have the `title` of their document too
([dashboard.md](dashboard.md)). A feed entry has `null` when no document exists at its path.

### Updates

The backend reads a document again when its size or modification time changes. A live edit
reaches the file within the flush delay of 2 seconds, the tree is built again, and the events
websocket sends `tree-changed`. The message carries nothing new: clients fetch the tree again,
as for every other change.

### Search

The backend has no search endpoint. Clients search the tree: they index `title`,
`meta.description` and `meta.tags`, leave out pages with `meta.searchExclude`, and can filter
by tag.

## Wiki config

`GET /wiki/config/` returns the settings of `mkdocs.yml` a renderer needs, for every role:

```json
{
  "siteName": "Tips & Tricks",
  "siteUrl": "https://wiki.example.com/",
  "useDirectoryUrls": true,
  "theme": {
    "name": "material",
    "features": ["content.code.copy"],
    "palette": [{ "scheme": "slate", "primary": "green", "accent": "light green" }]
  },
  "plugins": [{ "name": "search", "options": {} }],
  "markdownExtensions": [
    { "name": "admonition", "options": {} },
    { "name": "toc", "options": { "permalink": true, "baselevel": 2 } }
  ],
  "extraCss": ["stylesheets/extra.css"]
}
```

- `plugins` and `markdownExtensions` keep the order of `mkdocs.yml`. All forms MkDocs allows
  are read: a name, a map with one name and its options, or one mapping of all of them.
- Without a `plugins` key, `plugins` is `search` alone, the default of MkDocs.
- `palette` is always a list, `useDirectoryUrls` is `true` unless `mkdocs.yml` turns it off.
- A theme given as a plain name has no features and no palette.
- Python tags such as `!!python/name:material.extensions.emoji.twemoji` become the name after
  the tag.

Secrets never leave the server:

- Values tagged `!ENV` are left out.
- Keys whose name ends with `token`, `secret`, `password`, `passwd`, `apikey` (also
  `api_key`, `api-key`), `credential` or `credentials`, and the keys `key` and `auth`, are
  left out at any depth.
- Only the keys above are sent. `extra`, the repository settings, `nav`, hooks and the rest of
  `mkdocs.yml` stay on the server.

The backend reads `mkdocs.yml` only, not `zensical.toml`. If it cannot be read, the answer is
500.

The older `GET /mkdocs/config/` returns the whole `mkdocs.yml` without this filtering.
