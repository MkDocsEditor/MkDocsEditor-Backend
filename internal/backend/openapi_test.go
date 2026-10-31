package backend

import (
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The OpenAPI file is the contract for the clients: every route must be documented, and
// every documented operation must exist.
func TestOpenAPIMatchesRoutes(t *testing.T) {
	e := newRestTestEnv(t)

	data, err := os.ReadFile("../../openapi-description.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err = yaml.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}

	param := regexp.MustCompile(`\{[^}]+\}`)
	documented := map[string]bool{}
	for path, item := range spec.Paths {
		for method := range item {
			if method == "parameters" {
				continue
			}
			if path == "/.well-known/assetlinks.json" {
				// the well-known file is documented as it is fetched; the router adds the slash
				path += "/"
			}
			documented[strings.ToUpper(method)+" "+param.ReplaceAllString(path, ":p")] = true
		}
	}

	echoParam := regexp.MustCompile(`:[A-Za-z]+`)
	implemented := map[string]bool{}
	for _, r := range e.rs.echoRest.Routes() {
		if r.Method == "echo_route_not_found" || r.Method == http.MethodOptions {
			continue
		}
		implemented[r.Method+" "+echoParam.ReplaceAllString(r.Path, ":p")] = true
	}

	var missing, extra []string
	for k := range implemented {
		if !documented[k] {
			missing = append(missing, k)
		}
	}
	for k := range documented {
		if !implemented[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("routes missing in openapi-description.yaml: %v", missing)
	}
	if len(extra) > 0 {
		t.Errorf("operations in openapi-description.yaml without a route: %v", extra)
	}
}
