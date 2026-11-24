package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// One-time upload URLs for agents: a tool call carries its content as text, so an agent cannot
// upload a file of a few megabytes. request_upload hands out a URL the agent sends the file to
// from its shell. See docs/mcp.md.

const (
	urlParamToken = "token"

	// EndpointPathUpload is the prefix of the upload URLs, below APIPrefix
	EndpointPathUpload = "/uploads/"

	// defaultMaxUploadBytes is the largest file of an upload URL unless server.maxUploadBytes says otherwise
	defaultMaxUploadBytes = 64 << 20
	// uploadTicketTTL is how long an upload URL works
	uploadTicketTTL = 10 * time.Minute
	// maxUploadTickets limits the open URLs per account, and in total
	maxUploadTicketsPerUser = 10
	maxUploadTickets        = 200
)

type (
	mcpRequestUploadInput struct {
		Path     string `json:"path" jsonschema:"file path relative to the wiki root (like guide/img/plot.png), or relative to document"`
		Document string `json:"document,omitempty" jsonschema:"page the path is relative to, like a link in it"`
		Replace  bool   `json:"replace,omitempty" jsonschema:"allow replacing the file if it exists"`
	}

	// uploadTicket is an upload URL: the target, and who asked for it
	uploadTicket struct {
		rel     string
		replace bool
		caller  *mcpCaller
		expires time.Time
	}

	// uploadTickets holds the open upload URLs in memory: they do not survive a restart
	uploadTickets struct {
		mu      sync.Mutex
		tickets map[string]*uploadTicket
	}
)

// add stores a ticket and returns its secret, or an error if the caller has too many open
func (u *uploadTickets) add(tk *uploadTicket, now time.Time) (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	secret := base64.RawURLEncoding.EncodeToString(raw[:])
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.tickets == nil {
		u.tickets = map[string]*uploadTicket{}
	}
	own := 0
	for k, other := range u.tickets {
		switch {
		case now.After(other.expires):
			delete(u.tickets, k)
		case other.caller.user.ID == tk.caller.user.ID:
			own++
		}
	}
	if own >= maxUploadTicketsPerUser || len(u.tickets) >= maxUploadTickets {
		return "", errors.New("too many open upload URLs: use or wait for the earlier ones")
	}
	u.tickets[secret] = tk
	return secret, nil
}

// take removes and returns the ticket of a secret if it is still valid. A ticket works once.
func (u *uploadTickets) take(secret string, now time.Time) *uploadTicket {
	u.mu.Lock()
	defer u.mu.Unlock()
	tk := u.tickets[secret]
	delete(u.tickets, secret)
	if tk == nil || now.After(tk.expires) {
		return nil
	}
	return tk
}

func (t *mcpTools) requestUpload(_ context.Context, _ *mcp.CallToolRequest, in mcpRequestUploadInput) (*mcp.CallToolResult, any, error) {
	rel, err := t.uploadTarget(in.Path, in.Document)
	if err != nil {
		return nil, nil, err
	}
	tm := t.rs.treeManager
	if existing := tm.GetResource(tm.IDOfRelPath(rel)); existing != nil && !in.Replace {
		return nil, nil, fmt.Errorf("%s exists (%s): pass replace to overwrite it", rel, formatBytes(int(existing.Filesize)))
	}
	if err = t.allowWrite(); err != nil {
		return nil, nil, err
	}
	now := time.Now()
	secret, err := t.rs.uploads.add(&uploadTicket{rel: rel, replace: in.Replace, caller: t.caller, expires: now.Add(uploadTicketTTL)}, now)
	if err != nil {
		return nil, nil, err
	}
	// no trailing slash: curl -T appends the name of the local file to a URL that ends in one
	url := t.caller.baseURL + EndpointPathUpload + secret
	return text(fmt.Sprintf("Upload %s (up to %s) with:\n\ncurl -sS -T <file> '%s'\n\nThe URL works once and for %d minutes. "+
		"The answer of the server says whether it worked.", rel, formatBytes(int(t.rs.maxUploadBytes())), url, int(uploadTicketTTL.Minutes()))), nil, nil
}

// maxUploadBytes is server.maxUploadBytes, default 64 MiB
func (rs *RestService) maxUploadBytes() int64 {
	if rs.server.MaxUploadBytes > 0 {
		return rs.server.MaxUploadBytes
	}
	return defaultMaxUploadBytes
}

// spoolUpload writes a request body to a temporary file in the docs folder (the same disk as
// the result, and not a possibly memory backed /tmp), so a large file is not held in memory and
// a slow upload does not hold the lock of the tree. Names with a leading dot are no wiki
// items. The caller closes and removes the file.
func (rs *RestService) spoolUpload(body io.Reader) (*os.File, int64, error) {
	f, err := os.CreateTemp(rs.treeManager.RootPath(), ".upload-*")
	if err != nil {
		return nil, 0, err
	}
	n, err := io.Copy(f, body)
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, 0, err
	}
	return f, n, nil
}

// PUT /uploads/:token/
// The secret in the URL is the credential: the request carries no login.
func (rs *RestService) putUpload(c *echo.Context) error {
	tk := rs.uploads.take(c.Param(urlParamToken), time.Now())
	if tk == nil {
		return apiError(c, http.StatusNotFound, CodeNotFound, "this upload URL is unknown, expired or already used: ask for a new one", nil)
	}
	// the account may have lost its right since the URL was issued
	user := tk.caller.user
	if rs.authenticator.Enabled() {
		if user = rs.authenticator.UserByID(user.ID); user == nil || !user.CanWrite() {
			return apiError(c, http.StatusForbidden, CodeForbidden, "the account may not change content any more", nil)
		}
	}
	caller := *tk.caller
	caller.user = user
	limit := rs.maxUploadBytes()
	f, size, err := rs.spoolUpload(http.MaxBytesReader(c.Response(), c.Request().Body, limit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return apiError(c, http.StatusRequestEntityTooLarge, CodePayloadTooLarge, "the file is too large", map[string]any{"maxBytes": limit})
		}
		log.Printf("PUT %s: %v", c.Request().URL.Path, err)
		return apiError(c, http.StatusBadRequest, CodeBadRequest, "cannot read the request body", nil)
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}()
	if size == 0 {
		return apiError(c, http.StatusBadRequest, CodeBadRequest, "the file is empty", nil)
	}
	tools := &mcpTools{rs: rs, caller: &caller}
	// the tree may have changed since the URL was issued
	if _, err = tools.uploadTarget(tk.rel, ""); err != nil {
		return apiError(c, http.StatusConflict, CodeConflict, err.Error(), nil)
	}
	msg, err := tools.storeResource(tk.rel, f, tk.replace)
	if err != nil {
		if strings.Contains(err.Error(), "exists") {
			return apiError(c, http.StatusConflict, CodeConflict, err.Error(), nil)
		}
		return rs.ReturnError(c, err)
	}
	return c.String(http.StatusOK, msg+"\n")
}
