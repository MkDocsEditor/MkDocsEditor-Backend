package wiki

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// liveConfig is the mkdocs.yml of the family wiki, plus forms and secrets it does not have
const liveConfig = `site_name: 'Tips & Tricks'
site_url: 'https://wiki.markusressel.de/view/'
repo_url: 'https://git.example.com/wiki'
theme:
  name: 'material'
  custom_dir: 'theme'
  features:
    - content.code.copy
  palette:
    scheme: slate
    primary: green
    accent: 'light green'
extra_css:
  - stylesheets/codehilite.css
extra:
  analytics_key: abc
plugins:
  - search
  - git-committers:
      repository: markus/wiki
      token: !ENV GH_TOKEN
      enabled: !ENV [CI, false]
  - tags:
      tags_file: tags.md
markdown_extensions:
  - admonition
  - codehilite:
      guess_lang: false
      linenums: true
  - toc:
      permalink: true
      baselevel: 2
  - pymdownx.emoji:
      emoji_index: !!python/name:material.extensions.emoji.twemoji
      api_key: secret
  - pymdownx.tasklist:
      custom_checkbox: true
`

func TestReadWikiConfig(t *testing.T) {
	file := filepath.Join(t.TempDir(), "mkdocs.yml")
	if err := os.WriteFile(file, []byte(liveConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	config, err := ReadWikiConfig(file)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(config)
	got := string(data)
	for _, secret := range []string{"GH_TOKEN", "secret", "abc", "git.example.com", "token", "custom_dir", "enabled"} {
		if strings.Contains(got, secret) {
			t.Errorf("%q is in %s", secret, got)
		}
	}
	want := `{"siteName":"Tips \u0026 Tricks","siteUrl":"https://wiki.markusressel.de/view/","useDirectoryUrls":true,` +
		`"theme":{"name":"material","features":["content.code.copy"],"palette":[{"accent":"light green","primary":"green","scheme":"slate"}]},` +
		`"plugins":[{"name":"search","options":{}},{"name":"git-committers","options":{"repository":"markus/wiki"}},{"name":"tags","options":{"tags_file":"tags.md"}}],` +
		`"markdownExtensions":[{"name":"admonition","options":{}},{"name":"codehilite","options":{"guess_lang":false,"linenums":true}},` +
		`{"name":"toc","options":{"baselevel":2,"permalink":true}},{"name":"pymdownx.emoji","options":{"emoji_index":"material.extensions.emoji.twemoji"}},` +
		`{"name":"pymdownx.tasklist","options":{"custom_checkbox":true}}],"extraCss":["stylesheets/codehilite.css"]}`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}

	// the defaults: search, directory URLs, a theme as a plain name, plugins as a mapping
	if err := os.WriteFile(file, []byte("site_name: x\ntheme: readthedocs\nuse_directory_urls: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	config, err = ReadWikiConfig(file)
	if err != nil || config.Theme.Name != "readthedocs" || config.UseDirectoryURLs || len(config.Plugins) != 1 || config.Plugins[0].Name != "search" ||
		config.MarkdownExtensions == nil || config.Theme.Features == nil {
		t.Errorf("defaults = %+v, %v", config, err)
	}
	if err := os.WriteFile(file, []byte("plugins:\n  tags: {}\n  blog:\n    post_dir: posts\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if config, _ = ReadWikiConfig(file); len(config.Plugins) != 2 || config.Plugins[1].Options["post_dir"] != "posts" {
		t.Errorf("mapping = %+v", config.Plugins)
	}
}
