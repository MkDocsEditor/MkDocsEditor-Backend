package api

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/collab"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// wait_for_change: an agent that waits for a person to change a page. See docs/mcp.md.

const (
	// defaultWaitSeconds and maxWaitSeconds bound a wait_for_change. Below the timeouts of
	// common reverse proxies (60 s) and of MCP clients.
	defaultWaitSeconds = 20
	maxWaitSeconds     = 30
	// maxWaitsPerCaller is the most wait_for_change calls of one app token or account at once
	maxWaitsPerCaller = 2
)

// waitCounter counts running waits per caller. The zero value is ready.
type waitCounter struct {
	mu      sync.Mutex
	running map[string]int
}

// acquire counts a wait of a caller, false if the caller has too many
func (w *waitCounter) acquire(key string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.running == nil {
		w.running = map[string]int{}
	}
	if w.running[key] >= maxWaitsPerCaller {
		return false
	}
	w.running[key]++
	return true
}

func (w *waitCounter) release(key string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.running[key]--; w.running[key] <= 0 {
		delete(w.running, key)
	}
}

type mcpWaitInput struct {
	Document string `json:"document" jsonschema:"page path relative to the wiki root, like guide/setup.md, or the page id"`
	Since    string `json:"since,omitempty" jsonschema:"revision to wait for a change of; default: what you last read or wrote of the page"`
	Timeout  int    `json:"timeout,omitempty" jsonschema:"seconds to wait at most, 1 to 30, default 20"`
}

// waitForChangeDescription is the description of wait_for_change
const waitForChangeDescription = "Wait until someone changes a page that people have open, then show the change like " +
	"changes_since. Use it only when you wait for a person, for example after you asked them to edit something; " +
	"otherwise use changes_since. Returns at once if the page changed since you last read it, and after timeout " +
	"seconds (at most 30) without a change. Fails if nobody has the page open."

func (t *mcpTools) waitForChange(ctx context.Context, req *mcp.CallToolRequest, in mcpWaitInput) (*mcp.CallToolResult, any, error) {
	d, err := t.document(in.Document)
	if err != nil {
		return nil, nil, err
	}
	timeout := in.Timeout
	if timeout == 0 {
		timeout = defaultWaitSeconds
	}
	if timeout < 1 || timeout > maxWaitSeconds {
		return nil, nil, fmt.Errorf("timeout must be 1 to %d seconds", maxWaitSeconds)
	}
	key := t.caller.rateKey()
	if err = t.rs.authenticator.AllowWaitForChange(key, t.caller.info.IP); err != nil {
		var rl *auth.RateLimitError
		if errors.As(err, &rl) {
			return nil, nil, fmt.Errorf("too many waits, retry in %d seconds", rl.Seconds())
		}
		return nil, nil, err
	}
	if !t.rs.mcpWaits.acquire(key) {
		return nil, nil, fmt.Errorf("at most %d waits at once", maxWaitsPerCaller)
	}
	defer t.rs.mcpWaits.release(key)

	// the revision to wait for a change of: since, else what the caller last saw, else now
	since := in.Since
	baseline := since
	if since != "" {
		heads, err := collab.ParseRevision(since)
		if err != nil {
			return nil, nil, contentToolError(err)
		}
		baseline = collab.FormatRevision(heads)
	} else if seen, err := t.rs.sync.Activity().LastSeen(t.caller.seenKey(), d.RelPath); err != nil {
		return nil, nil, err
	} else if seen != nil {
		baseline = seen.Token
	}
	if baseline == "" {
		if baseline, err = t.rs.sync.OpenRevision(d); err != nil {
			return nil, nil, waitToolError(err)
		}
		since = baseline
	}
	waitCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	revision, changed, err := t.rs.sync.WaitForChange(waitCtx, d, baseline)
	if err != nil {
		return nil, nil, waitToolError(err)
	}
	if !changed {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return text(fmt.Sprintf("path: %s\nrevision: %s\nno changes within %d seconds", d.RelPath, revision, timeout)), nil, nil
	}
	return t.changesSince(ctx, req, mcpChangesInput{Document: in.Document, Since: since})
}

// waitToolError turns an error of a wait into a message for the agent
func waitToolError(err error) error {
	if errors.Is(err, collab.ErrNobodyOpen) {
		return errors.New("nobody has this page open: there is nobody to wait for; use changes_since later")
	}
	return err
}
