package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/gitstore"
)

func TestBlame(t *testing.T) {
	e := newGitEnv(t, gitstore.CommitterOptions{})
	rel := "guide/setup.md"
	id := e.docID(rel)
	e.appendCommitted(id, rel, "bob", "line bob\n")
	h := e.history(id)

	// not committed yet: an agent's line and one alice types
	token := e.appToken("bob", "claude", auth.ScopeWrite)
	expectCode(t, e.doToken(http.MethodPost, "/document/"+id+"/edits/", token, map[string]any{
		"edits": []map[string]string{{"old": "line bob\n", "new": "line bob\nline agent\n"}},
	}), http.StatusOK, "")
	a := newTestClient(t, e, "alice")
	a.connect(id)
	a.waitFor("agent edit", func() bool { return strings.Contains(a.textLocked(), "line agent") })
	a.appendText("line alice\n")
	eventually(t, "server", func() bool { return e.serverText(rel) == a.text() })

	var res BlameResponse
	eventually(t, "blame", func() bool {
		r := e.do(http.MethodGet, "/document/"+id+"/blame/", "carol")
		if r.Code != http.StatusOK {
			t.Fatalf("blame %d %s", r.Code, r.Body.String())
		}
		res = BlameResponse{}
		if err := json.Unmarshal(r.bytes(), &res); err != nil {
			t.Fatal(err)
		}
		return res.Lines == 6
	})
	var got []string
	for _, b := range res.Blocks {
		s := fmt.Sprintf("%d-%d", b.Start, b.End)
		if b.Pending {
			s += " pending"
		} else {
			s += " " + (*b.Revision)[:7] + " " + *b.Message
		}
		if b.Author != nil {
			s += " " + b.Author.Username
		}
		s += " " + strings.Join(b.Agents, ",")
		got = append(got, s)
	}
	want := []string{
		"1-3 " + h[1].Revision[:7] + " Initial  ",
		"4-4 " + h[0].Revision[:7] + " Update guide/setup.md bob ",
		"5-5 pending bob claude",
		"6-6 pending alice ",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("blocks:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if res.Revision == "" || res.Blocks[1].Author.DisplayName == "" || res.Blocks[1].Time == nil {
		t.Errorf("blame %+v", res)
	}
}
