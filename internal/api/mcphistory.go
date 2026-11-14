package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/activity"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCP tools for the revision history: what changed, who changed it, and undoing an agent's own
// commits. See docs/mcp.md.

const (
	mcpHistoryDefaultLimit = 20
	mcpHistoryMaxLimit     = 100
)

type (
	mcpHistoryInput struct {
		Document string `json:"document" jsonschema:"page path relative to the wiki root, like guide/setup.md, or the page id"`
		Limit    int    `json:"limit,omitempty" jsonschema:"maximum number of commits, 1 to 100, default 20"`
	}

	mcpRevisionInput struct {
		Document string `json:"document" jsonschema:"page path relative to the wiki root, like guide/setup.md, or the page id"`
		Revision string `json:"revision" jsonschema:"commit from read_history"`
	}

	mcpChangesInput struct {
		Document string `json:"document" jsonschema:"page path relative to the wiki root, like guide/setup.md, or the page id"`
		Since    string `json:"since,omitempty" jsonschema:"revision from read_document; default: what you last read or wrote of the page"`
	}
)

// seenKey is the key of what the caller saw of pages: per app token, so an agent does not mark
// pages as seen for its person
func (c *mcpCaller) seenKey() string {
	if c.token != nil {
		return "token:" + c.token.ID
	}
	return "mcp:" + c.user.ID
}

// markSeen remembers the text of a revision as seen by the caller, for changes_since
func (t *mcpTools) markSeen(d *wiki.Document, revision string, text *string) {
	if text == nil {
		s, _, err := t.rs.sync.ReadContentAt(d, revision)
		if err != nil {
			return
		}
		text = &s
	}
	_, _ = t.rs.sync.Activity().SetSeen(t.caller.seenKey(), d.RelPath, revision, *text)
}

// advanceSeen moves what the caller saw over its own change from before to after, but only
// if it had seen before: otherwise it would skip changes of others it never read
func (t *mcpTools) advanceSeen(d *wiki.Document, before, after string) {
	seen, err := t.rs.sync.Activity().LastSeen(t.caller.seenKey(), d.RelPath)
	if err == nil && seen != nil && seen.Token == before {
		t.markSeen(d, after, nil)
	}
}

// historyRepo returns the git repository, or an error for the agent without git
func (t *mcpTools) historyRepo() error {
	if t.rs.git == nil {
		return errors.New("this wiki has no revision history (git is disabled)")
	}
	return nil
}

func formatHistoryAuthor(a HistoryAuthor) string {
	if a.DisplayName != "" {
		return a.DisplayName
	}
	return a.Name
}

func (t *mcpTools) readHistory(_ context.Context, _ *mcp.CallToolRequest, in mcpHistoryInput) (*mcp.CallToolResult, any, error) {
	if err := t.historyRepo(); err != nil {
		return nil, nil, err
	}
	d, err := t.document(in.Document)
	if err != nil {
		return nil, nil, err
	}
	limit := in.Limit
	if limit <= 0 {
		limit = mcpHistoryDefaultLimit
	}
	repo := t.rs.git.Repo()
	commits, err := repo.FileHistory(d.RelPath, min(limit, mcpHistoryMaxLimit))
	if err != nil {
		return nil, nil, err
	}
	summaries, err := t.rs.commitSummaries(repo, commits)
	if err != nil {
		return nil, nil, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "path: %s\nnewest first; uncommitted edits are not listed, see changes_since\n", d.RelPath)
	for i, cm := range commits {
		e := t.rs.historyEntry(cm, summaries[i])
		who := formatHistoryAuthor(e.Author)
		for _, co := range e.CoAuthors {
			who += ", " + formatHistoryAuthor(co)
		}
		fmt.Fprintf(&b, "\n%s %s by %s", shortRevision(e.Revision), e.Time.UTC().Format("2006-01-02 15:04"), who)
		for _, a := range e.Agents {
			fmt.Fprintf(&b, " (agent %s)", a.Name)
		}
		fmt.Fprintf(&b, ": %s, +%d -%d lines", e.Message, e.Summary.LinesAdded, e.Summary.LinesRemoved)
		if len(e.Summary.Headings) > 0 {
			fmt.Fprintf(&b, " in %s", strings.Join(e.Summary.Headings, ", "))
		}
		if e.Path != d.RelPath {
			fmt.Fprintf(&b, " (as %s)", e.Path)
		}
	}
	if len(commits) == 0 {
		b.WriteString("\nno commits yet")
	}
	return text(b.String()), nil, nil
}

func (t *mcpTools) readRevision(_ context.Context, _ *mcp.CallToolRequest, in mcpRevisionInput) (*mcp.CallToolResult, any, error) {
	if err := t.historyRepo(); err != nil {
		return nil, nil, err
	}
	d, err := t.document(in.Document)
	if err != nil {
		return nil, nil, err
	}
	content, _, err := t.rs.revisionText(d, d.RelPath, t.rs.git.Repo(), strings.TrimSpace(in.Revision))
	if errors.Is(err, ErrUnknownRevision) || in.Revision == RevisionCurrent {
		return nil, nil, fmt.Errorf("revision %q is not in the history of %s: use read_history", in.Revision, d.RelPath)
	}
	if err != nil {
		return nil, nil, err
	}
	return text(fmt.Sprintf("path: %s\nrevision: %s\n---\n%s", d.RelPath, in.Revision, content)), nil, nil
}

// formatHunks writes hunks like a unified diff, with their heading
func formatHunks(b *strings.Builder, hunks []activity.Hunk) {
	for _, h := range hunks {
		fmt.Fprintf(b, "\n@@ -%d,%d +%d,%d @@", h.OldStart, h.OldLines, h.NewStart, h.NewLines)
		if h.Heading != nil {
			fmt.Fprintf(b, " %s", *h.Heading)
		}
		for _, l := range h.Lines {
			b.WriteString("\n" + l.Op + l.Text)
		}
	}
}

func (t *mcpTools) changesSince(_ context.Context, _ *mcp.CallToolRequest, in mcpChangesInput) (*mcp.CallToolResult, any, error) {
	d, err := t.document(in.Document)
	if err != nil {
		return nil, nil, err
	}
	current, revision, err := t.rs.sync.ReadContent(d)
	if err != nil {
		return nil, nil, err
	}
	defer t.markSeen(d, revision, &current)
	since := strings.TrimSpace(in.Since)
	var base string
	var sinceTime time.Time
	if since == "" {
		seen, err := t.rs.sync.Activity().LastSeen(t.caller.seenKey(), d.RelPath)
		if err != nil {
			return nil, nil, err
		}
		if seen == nil {
			return text(fmt.Sprintf("path: %s\nrevision: %s\nyou have not read this page before: read_document shows it; "+
				"from now on changes_since shows what changed since", d.RelPath, revision)), nil, nil
		}
		since, base, sinceTime = seen.Token, seen.Text, seen.Time
	} else if base, sinceTime, err = t.rs.sync.ReadContentAt(d, since); err != nil {
		return nil, nil, contentToolError(err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "path: %s\nsince: %s\nrevision: %s", d.RelPath, since, revision)
	if base == current {
		b.WriteString("\nno changes")
		return text(b.String()), nil, nil
	}
	var who []string
	for _, a := range t.rs.changeAuthors(d, since, sinceTime) {
		name := a.DisplayName
		if len(a.Agents) > 0 {
			name += " (agent " + strings.Join(a.Agents, ", ") + ")"
		}
		who = append(who, name)
	}
	sum := activity.Summarize(base, current)
	fmt.Fprintf(&b, "\nchanged by: %s\n+%d -%d lines", strings.Join(who, ", "), sum.LinesAdded, sum.LinesRemoved)
	formatHunks(&b, activity.Hunks(base, current))
	return text(b.String()), nil, nil
}

func (t *mcpTools) revertChange(_ context.Context, _ *mcp.CallToolRequest, in mcpRevisionInput) (*mcp.CallToolResult, any, error) {
	if err := t.historyRepo(); err != nil {
		return nil, nil, err
	}
	d, err := t.document(in.Document)
	if err != nil {
		return nil, nil, err
	}
	token := t.caller.token
	if token == nil {
		return nil, nil, errors.New("revert_change undoes commits of your own app token, and this connection has none")
	}
	commit, before, after, err := revisionChange(t.rs.git.Repo(), d.RelPath, strings.TrimSpace(in.Revision))
	switch {
	case errors.Is(err, ErrUnknownRevision):
		return nil, nil, fmt.Errorf("revision %q is not in the history of %s: use read_history", in.Revision, d.RelPath)
	case err != nil:
		return nil, nil, err
	}
	e := t.rs.historyEntry(*commit, HistorySummary{})
	own := e.Author.AccountID == t.caller.user.ID && len(e.CoAuthors) == 0 && len(e.Agents) == 1 &&
		e.Agents[0].Name == token.Name && e.Agents[0].AccountID == t.caller.user.ID
	if !own {
		return nil, nil, fmt.Errorf("you can only revert commits made with your own app token %q; %s is by %s: "+
			"ask a person to undo it, or change the page with edit_document", token.Name, shortRevision(commit.Revision), formatHistoryAuthor(e.Author))
	}
	if err = t.allowWrite(); err != nil {
		return nil, nil, err
	}
	entry, err := t.rs.revert(d, d.RelPath, commit, before, after, t.caller.user, t.caller.agent())
	var conflict *revertError
	switch {
	case errors.As(err, &conflict):
		return nil, nil, fmt.Errorf("nothing changed: %w; read_document and fix it with edit_document", err)
	case errors.Is(err, errNothingToRevert):
		return nil, nil, errors.New("nothing changed: the change of this commit is not in the page anymore")
	case err != nil:
		return nil, nil, contentToolError(err)
	}
	_, revision, err := t.rs.sync.ReadContent(d)
	if err == nil {
		t.markSeen(d, revision, nil)
	}
	return text(fmt.Sprintf("reverted %s on %s as commit %s (+%d -%d lines)\nrevision: %s",
		shortRevision(commit.Revision), d.RelPath, shortRevision(entry.Revision), entry.Summary.LinesAdded, entry.Summary.LinesRemoved, revision)), nil, nil
}
