package wiki

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// WikiConfig is the part of mkdocs.yml a renderer needs, without secrets, see docs/page-meta.md
type WikiConfig struct {
	SiteName           string      `json:"siteName"`
	SiteURL            string      `json:"siteUrl"`
	UseDirectoryURLs   bool        `json:"useDirectoryUrls"`
	Theme              ThemeConfig `json:"theme"`
	Plugins            []Extension `json:"plugins"`
	MarkdownExtensions []Extension `json:"markdownExtensions"`
	ExtraCSS           []string    `json:"extraCss"`
}

type ThemeConfig struct {
	Name     string           `json:"name"`
	Features []string         `json:"features"`
	Palette  []map[string]any `json:"palette"`
}

// Extension is a plugin or a Markdown extension with its options
type Extension struct {
	Name    string         `json:"name"`
	Options map[string]any `json:"options"`
}

// secretSuffixes end option names that are never sent, compared in lower case without "_"
// and "-": github_token, api-key, password. "key" and "auth" alone too.
var secretSuffixes = []string{"token", "secret", "password", "passwd", "apikey", "credentials", "credential"}

func isSecretKey(k string) bool {
	k = strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(k))
	if k == "key" || k == "auth" {
		return true
	}
	for _, s := range secretSuffixes {
		if strings.HasSuffix(k, s) {
			return true
		}
	}
	return false
}

// ReadWikiConfig reads the renderer settings of mkdocs.yml
func ReadWikiConfig(configFile string) (*WikiConfig, error) {
	data, err := os.ReadFile(configFile)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err = yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("mkdocs.yml is not a mapping")
	}
	top := map[string]*yaml.Node{}
	root := doc.Content[0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		top[root.Content[i].Value] = root.Content[i+1]
	}

	config := &WikiConfig{UseDirectoryURLs: true, Theme: ThemeConfig{Features: []string{}, Palette: []map[string]any{}},
		ExtraCSS: []string{}, MarkdownExtensions: []Extension{}}
	config.SiteName, _ = scalar(top["site_name"]).(string)
	config.SiteURL, _ = scalar(top["site_url"]).(string)
	if v, ok := scalar(top["use_directory_urls"]).(bool); ok {
		config.UseDirectoryURLs = v
	}
	if theme := top["theme"]; theme != nil {
		switch theme.Kind {
		case yaml.ScalarNode:
			config.Theme.Name, _ = scalar(theme).(string)
		case yaml.MappingNode:
			t := asMap(theme)
			config.Theme.Name, _ = t["name"].(string)
			config.Theme.Features = stringList(t["features"])
			switch p := t["palette"].(type) {
			case map[string]any:
				config.Theme.Palette = append(config.Theme.Palette, p)
			case []any:
				for _, e := range p {
					if m, ok := e.(map[string]any); ok {
						config.Theme.Palette = append(config.Theme.Palette, m)
					}
				}
			}
		}
	}
	if plugins, ok := top["plugins"]; ok {
		config.Plugins = extensions(plugins)
	} else {
		// the default of MkDocs
		config.Plugins = []Extension{{Name: "search", Options: map[string]any{}}}
	}
	config.MarkdownExtensions = extensions(top["markdown_extensions"])
	config.ExtraCSS = stringList(value(top["extra_css"]))
	return config, nil
}

// extensions reads the forms MkDocs allows: a list of names and one-key maps, or a mapping
func extensions(n *yaml.Node) []Extension {
	out := []Extension{}
	if n == nil {
		return out
	}
	add := func(name string, options *yaml.Node) {
		opts, _ := value(options).(map[string]any)
		if opts == nil {
			opts = map[string]any{}
		}
		out = append(out, Extension{Name: name, Options: opts})
	}
	switch n.Kind {
	case yaml.SequenceNode:
		for _, e := range n.Content {
			switch e.Kind {
			case yaml.ScalarNode:
				add(e.Value, nil)
			case yaml.MappingNode:
				for i := 0; i+1 < len(e.Content); i += 2 {
					add(e.Content[i].Value, e.Content[i+1])
				}
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			add(n.Content[i].Value, n.Content[i+1])
		}
	}
	return out
}

func asMap(n *yaml.Node) map[string]any {
	m, _ := value(n).(map[string]any)
	return m
}

func stringList(v any) []string {
	out := []string{}
	if list, ok := v.([]any); ok {
		for _, e := range list {
			if e != nil {
				out = append(out, fmt.Sprint(e))
			}
		}
	}
	return out
}

func scalar(n *yaml.Node) any {
	if n == nil || n.Kind != yaml.ScalarNode {
		return nil
	}
	return value(n)
}

// errDropped marks a value that is not sent
var errDropped = errors.New("dropped")

// value converts a node to JSON values. Values tagged !ENV and keys that look like secrets
// are left out, other custom tags such as !!python/name:x become the text after the tag.
func value(n *yaml.Node) any {
	v, err := convert(n)
	if err != nil {
		return nil
	}
	return v
}

func convert(n *yaml.Node) (any, error) {
	if n == nil {
		return nil, nil
	}
	if n.Kind == yaml.AliasNode {
		return convert(n.Alias)
	}
	if n.Tag == "!ENV" || strings.HasPrefix(n.Tag, "!ENV") {
		return nil, errDropped
	}
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return nil, nil
		}
		return convert(n.Content[0])
	case yaml.MappingNode:
		m := map[string]any{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i].Value
			if isSecretKey(k) {
				continue
			}
			v, err := convert(n.Content[i+1])
			if errors.Is(err, errDropped) {
				continue
			}
			m[k] = v
		}
		return m, nil
	case yaml.SequenceNode:
		list := []any{}
		for _, e := range n.Content {
			v, err := convert(e)
			if errors.Is(err, errDropped) {
				continue
			}
			list = append(list, v)
		}
		return list, nil
	}
	switch n.Tag {
	case "!!str", "":
		return n.Value, nil
	case "!!int":
		if i, err := strconv.ParseInt(n.Value, 0, 64); err == nil {
			return i, nil
		}
	case "!!float":
		if f, err := strconv.ParseFloat(n.Value, 64); err == nil {
			return f, nil
		}
	case "!!bool":
		var b bool
		if err := n.Decode(&b); err == nil {
			return b, nil
		}
	case "!!null":
		return nil, nil
	}
	// !!python/name:module.function and other custom tags: the text after the tag
	for _, prefix := range []string{"!!python/name:", "tag:yaml.org,2002:python/name:"} {
		if name, ok := strings.CutPrefix(n.Tag, prefix); ok {
			return name + n.Value, nil
		}
	}
	return n.Value, nil
}
