package backend

import (
	"context"
	"errors"
	"fmt"

	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"gopkg.in/yaml.v3"
)

const (
	urlParamParentID = "parentID"
	urlParamID       = "id"
	urlParamName     = "name"
	urlParamRevision = "revision"
	jsonIndent       = "  "

	EndpointPathAlive = "/alive/"
)

type (
	ErrorResult struct {
		Name    string         `json:"name"`
		Code    string         `json:"code,omitempty"`
		Message string         `json:"message"`
		Details map[string]any `json:"details,omitempty"`
	}

	// NewItemRequest is the body of POST /section/ and POST /document/. form and xml bodies
	// are bound too, for older clients.
	NewItemRequest struct {
		Parent string `json:"parent" xml:"parent" form:"parent"`
		Name   string `json:"name" xml:"name" form:"name"`
	}

	// RenameRequest is the body of the rename endpoints
	RenameRequest struct {
		Name string `json:"name" xml:"name" form:"name"`
	}
)

type RestService struct {
	echoRest       *echo.Echo
	server         configuration.ServerConfiguration
	allowedOrigins []string
	trustedProxies []*net.IPNet
	authenticator  *auth.Service
	treeManager    *TreeManager
	sync           *SyncServer
	git            *GitCommitter
	refs           *ReferenceIndex
	// thumbnailSlots limits concurrent image decoding
	thumbnailSlots chan struct{}
}

func NewRestService(
	server configuration.ServerConfiguration,
	treeManager *TreeManager,
	syncServer *SyncServer,
	authenticator *auth.Service,
) *RestService {
	trusted, err := ParseTrustedProxies(server.TrustedProxies)
	if err != nil {
		log.Printf("Ignoring server.trustedProxies: %v", err)
	}
	rs := &RestService{
		server:         server,
		treeManager:    treeManager,
		sync:           syncServer,
		authenticator:  authenticator,
		allowedOrigins: server.CORS.AllowedOrigins,
		trustedProxies: trusted,
		refs:           NewReferenceIndex(treeManager, syncServer),
		thumbnailSlots: make(chan struct{}, maxConcurrentThumbnails),
	}
	rs.echoRest = rs.createRestService()
	return rs
}

// SetGitCommitter makes tree changes over REST commit to git. Call it before Start.
func (rs *RestService) SetGitCommitter(c *GitCommitter) {
	rs.git = c
}

// commitTree commits a tree change of the requesting user, if git is enabled.
// format gets the paths relative to the docs folder.
func (rs *RestService) commitTree(c echo.Context, format string, absPaths ...string) {
	if rs.git == nil {
		return
	}
	rels := make([]string, 0, len(absPaths))
	args := make([]any, 0, len(absPaths))
	for _, p := range absPaths {
		rel, err := filepath.Rel(rs.treeManager.RootPath(), p)
		if err != nil || !filepath.IsLocal(rel) {
			log.Printf("Not committing %s: outside the docs folder", p)
			return
		}
		rel = filepath.ToSlash(rel)
		rels = append(rels, rel)
		args = append(args, rel)
	}
	rs.git.CommitTreeChange(UserFromContext(c), fmt.Sprintf(format, args...), rels...)
}

// unescapePathParams decodes percent-encoded path parameters. Echo matches routes on the raw
// path when it contains escapes, so "Strand%20(2).png" would otherwise arrive as is.
func unescapePathParams(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		values := c.ParamValues()
		decoded := make([]string, len(values))
		for i, v := range values {
			d, err := url.PathUnescape(v)
			if err != nil {
				return apiError(c, http.StatusBadRequest, CodeBadRequest, "invalid percent-encoding in the path", nil)
			}
			decoded[i] = d
		}
		c.SetParamValues(decoded...)
		return next(c)
	}
}

func (rs *RestService) createRestService() *echo.Echo {
	echoRest := echo.New()
	echoRest.HideBanner = true
	echoRest.IPExtractor = rs.ipExtractor()
	defaultErrorHandler := echoRest.DefaultHTTPErrorHandler
	echoRest.HTTPErrorHandler = func(err error, c echo.Context) {
		if errors.Is(err, errResponded) {
			return
		}
		defaultErrorHandler(err, c)
	}

	// Root level middleware
	echoRest.Pre(middleware.AddTrailingSlash())

	echoRest.Use(middleware.SecureWithConfig(middleware.SecureConfig{
		XSSProtection:         "0",
		ContentTypeNosniff:    "nosniff",
		XFrameOptions:         "DENY",
		ContentSecurityPolicy: "default-src 'none'; frame-ancestors 'none'",
		ReferrerPolicy:        "no-referrer",
	}))
	echoRest.Use(rs.securityHeaders)

	echoRest.Use(middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
		LogMethod:   true,
		LogURIPath:  true,
		LogStatus:   true,
		LogLatency:  true,
		LogError:    true,
		HandleError: true,
		LogValuesFunc: func(c echo.Context, v middleware.RequestLoggerValues) error {
			if v.Error != nil && !errors.Is(v.Error, errResponded) {
				log.Printf("%s %s %d %v error=%v", v.Method, v.URIPath, v.Status, v.Latency, v.Error)
			} else {
				log.Printf("%s %s %d %v", v.Method, v.URIPath, v.Status, v.Latency)
			}
			return nil
		},
	}))
	echoRest.Use(middleware.Recover())

	echoRest.Use(middleware.CORSWithConfig(corsConfig(rs.server.CORS)))

	// global auth
	echoRest.Use(rs.authMiddleware)
	echoRest.Use(unescapePathParams)

	rs.registerAuthRoutes(echoRest)
	rs.registerAccountRoutes(echoRest)
	rs.registerAdminRoutes(echoRest)
	rs.registerContentRoutes(echoRest)

	return echoRest
}

// registerAuthRoutes adds login, sessions, links and the public server information
func (rs *RestService) registerAuthRoutes(echoRest *echo.Echo) {
	echoRest.GET("/auth/status/", rs.authStatus)
	echoRest.POST("/auth/login/", rs.login)
	echoRest.POST("/auth/login/second-factor/", rs.loginSecondFactor)
	echoRest.POST("/auth/reauth/", rs.reauth)
	echoRest.POST("/auth/reauth/passkey/begin/", rs.reauthPasskeyBegin)
	echoRest.POST("/auth/reauth/passkey/finish/", rs.reauthPasskeyFinish)
	echoRest.POST("/auth/passkey/login/begin/", rs.passkeyLoginBegin)
	echoRest.POST("/auth/passkey/login/finish/", rs.passkeyLoginFinish)
	echoRest.POST("/auth/link/passkey/begin/", rs.linkPasskeyBegin)
	echoRest.POST("/auth/link/passkey/finish/", rs.linkPasskeyFinish)
	echoRest.GET("/.well-known/assetlinks.json/", rs.assetLinks)
	echoRest.POST("/auth/logout/", rs.logout)
	echoRest.POST("/auth/link/inspect/", rs.inspectLink)
	echoRest.POST("/auth/link/redeem/", rs.redeemLink)
	echoRest.POST("/auth/device-link/redeem/", rs.redeemDeviceLink)
	echoRest.GET("/server-info/", rs.serverInfo)
	echoRest.GET(EndpointPathWhoAmI, rs.whoAmI)
	echoRest.POST(EndpointPathWsTicket, rs.issueWsTicket)
}

// registerAccountRoutes adds the endpoints of the current user
func (rs *RestService) registerAccountRoutes(echoRest *echo.Echo) {
	echoRest.GET("/account/", rs.getAccount)
	echoRest.PATCH("/account/", rs.updateAccount)
	echoRest.PUT("/account/password/", rs.changePassword)
	echoRest.DELETE("/account/password/", rs.removePassword)
	echoRest.POST("/account/totp/", rs.beginTOTP)
	echoRest.POST("/account/totp/confirm/", rs.confirmTOTP)
	echoRest.DELETE("/account/totp/", rs.disableTOTP)
	echoRest.POST("/account/recovery-codes/", rs.regenerateRecoveryCodes)
	echoRest.GET("/account/passkeys/", rs.listPasskeys)
	echoRest.POST("/account/passkeys/begin/", rs.addPasskeyBegin)
	echoRest.POST("/account/passkeys/finish/", rs.addPasskeyFinish)
	echoRest.PATCH("/account/passkeys/:"+urlParamID+"/", rs.renamePasskey)
	echoRest.DELETE("/account/passkeys/:"+urlParamID+"/", rs.deletePasskey)
	echoRest.GET("/account/tokens/", rs.listAppTokens)
	echoRest.POST("/account/tokens/", rs.createAppToken)
	echoRest.DELETE("/account/tokens/:"+urlParamID+"/", rs.revokeAppToken)
	echoRest.GET("/account/device-links/", rs.listDeviceLinks)
	echoRest.POST("/account/device-links/", rs.createDeviceLink)
	echoRest.GET("/account/device-links/:"+urlParamID+"/", rs.getDeviceLink)
	echoRest.DELETE("/account/device-links/:"+urlParamID+"/", rs.revokeDeviceLink)
	echoRest.GET("/account/sessions/", rs.accountSessions)
	echoRest.DELETE("/account/sessions/:"+urlParamID+"/", rs.revokeAccountSession)
	echoRest.POST("/account/sessions/revoke-others/", rs.revokeOtherSessions)
}

// registerAdminRoutes adds the admin API
func (rs *RestService) registerAdminRoutes(echoRest *echo.Echo) {
	echoRest.GET("/admin/invites/", rs.listInvites)
	echoRest.POST("/admin/invites/", rs.createInvite)
	echoRest.DELETE("/admin/invites/:"+urlParamID+"/", rs.revokeInvite)
	echoRest.GET("/admin/users/", rs.adminListUsers)
	echoRest.PATCH("/admin/users/:"+urlParamID+"/", rs.adminUpdateUser)
	echoRest.DELETE("/admin/users/:"+urlParamID+"/", rs.adminDeleteUser)
	echoRest.GET("/admin/users/:"+urlParamID+"/sessions/", rs.adminUserSessions)
	echoRest.POST("/admin/users/:"+urlParamID+"/sessions/revoke/", rs.adminRevokeUserSessions)
	echoRest.POST("/admin/users/:"+urlParamID+"/reset-link/", rs.adminCreateResetLink)
	echoRest.DELETE("/admin/sessions/:"+urlParamID+"/", rs.adminRevokeSession)
	echoRest.GET("/admin/reset-links/", rs.adminListResetLinks)
	echoRest.DELETE("/admin/reset-links/:"+urlParamID+"/", rs.adminRevokeResetLink)
	echoRest.GET("/admin/settings/", rs.adminSettings)
	echoRest.PATCH("/admin/settings/", rs.adminUpdateSettings)
	echoRest.GET("/admin/audit/", rs.adminAudit)
}

// registerContentRoutes adds the endpoints of the wiki content
func (rs *RestService) registerContentRoutes(echoRest *echo.Echo) {
	echoRest.GET("/actors/", rs.getActors)
	echoRest.GET("/events/ws/", rs.handleEventsWebsocket)
	echoRest.GET(EndpointPathAlive, rs.isAlive)
	echoRest.GET("/references/broken/", rs.getBrokenReferences)
	echoRest.GET("/references/unused-resources/", rs.getUnusedResources)
	echoRest.GET("/link/", rs.getLink)
	echoRest.POST("/move/", rs.postMove)
	echoRest.GET("/moved/:"+urlParamID+"/", rs.getMoved)

	groupMkDocs := echoRest.Group("/mkdocs")
	groupSections := echoRest.Group("/section")
	groupDocuments := echoRest.Group("/document")
	groupResources := echoRest.Group("/resource")

	groupMkDocs.GET("/config/", rs.getMkDocsConfig)

	groupSections.GET("/", rs.getTree)
	groupSections.GET("/:"+urlParamID+"/", rs.describeItem(TypeSection))
	groupSections.GET("/:"+urlParamID+"/backlinks/", rs.getSectionBacklinks)
	groupSections.POST("/", rs.createSection)
	groupSections.PUT("/:"+urlParamID+"/", rs.renameItem(TypeSection))
	groupSections.DELETE("/:"+urlParamID+"/", rs.deleteItem(TypeSection))

	groupDocuments.GET("/:"+urlParamID+"/", rs.describeItem(TypeDocument))
	groupDocuments.GET("/:"+urlParamID+"/ws/", rs.handleWebsocket)
	groupDocuments.GET("/:"+urlParamID+"/content/", rs.getDocumentContent)
	groupDocuments.GET("/:"+urlParamID+"/references/", rs.getDocumentReferences)
	groupDocuments.GET("/:"+urlParamID+"/backlinks/", rs.getDocumentBacklinks)
	groupDocuments.GET("/:"+urlParamID+"/authorship/", rs.getDocumentAuthorship)
	groupDocuments.GET("/:"+urlParamID+"/history/", rs.getDocumentHistory)
	groupDocuments.GET("/:"+urlParamID+"/history/diff/", rs.getDocumentRevisionDiff)
	groupDocuments.GET("/:"+urlParamID+"/history/:"+urlParamRevision+"/content/", rs.getDocumentRevisionContent)
	groupDocuments.POST("/:"+urlParamID+"/history/:"+urlParamRevision+"/restore/", rs.restoreDocumentRevision)
	groupDocuments.POST("/", rs.createDocument)
	groupDocuments.PUT("/:"+urlParamID+"/", rs.renameItem(TypeDocument))
	groupDocuments.DELETE("/:"+urlParamID+"/", rs.deleteItem(TypeDocument))

	groupResources.GET("/:"+urlParamID+"/", rs.describeItem(TypeResource))
	groupResources.GET("/:"+urlParamID+"/content/", rs.getResourceContent)
	groupResources.GET("/:"+urlParamID+"/backlinks/", rs.getResourceBacklinks)
	groupResources.GET("/:"+urlParamID+"/thumbnail/", rs.getResourceThumbnail)
	groupResources.POST("/:"+urlParamParentID+"/:"+urlParamName+"/", rs.uploadNewResource)
	groupResources.PUT("/:"+urlParamID+"/", rs.renameItem(TypeResource))
	groupResources.DELETE("/:"+urlParamID+"/", rs.deleteItem(TypeResource))
}

// corsConfig builds the CORS configuration. With explicit origins, credentials are allowed,
// so browsers may send the Authorization header from those origins.
func corsConfig(conf configuration.CorsConfiguration) middleware.CORSConfig {
	c := middleware.CORSConfig{
		AllowOrigins: []string{"*"},
		AllowMethods: conf.AllowedMethods,
		AllowHeaders: []string{
			echo.HeaderAuthorization, echo.HeaderContentType, echo.HeaderAccept, "X-Requested-With",
		},
		ExposeHeaders: []string{echo.HeaderContentDisposition, "Retry-After"},
		MaxAge:        3600,
	}
	if len(c.AllowMethods) == 0 {
		c.AllowMethods = []string{http.MethodHead, http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodPost, http.MethodDelete}
	}
	if len(conf.AllowedOrigins) > 0 {
		c.AllowOrigins = conf.AllowedOrigins
		c.AllowCredentials = true
	}
	return c
}

// Start the REST service. It returns after Shutdown, or with the error if serving fails.
func (rs *RestService) Start() error {
	rs.refs.Warm()
	err := rs.echoRest.Start(net.JoinHostPort(rs.server.Host, strconv.Itoa(rs.server.Port)))
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown stops the REST service gracefully
func (rs *RestService) Shutdown(ctx context.Context) {
	if err := rs.echoRest.Shutdown(ctx); err != nil {
		log.Printf("Shutdown: %v", err)
	}
}

// returns an empty "ok" answer
func (rs *RestService) isAlive(c echo.Context) error {
	return c.NoContent(http.StatusOK)
}

func (rs *RestService) getMkDocsConfig(c echo.Context) error {
	data, err := os.ReadFile(rs.treeManager.ConfigFile())
	if err != nil {
		return rs.ReturnError(c, err)
	}

	var config map[string]any
	if err = yaml.Unmarshal(data, &config); err != nil {
		return rs.ReturnError(c, err)
	}
	return c.JSONPretty(http.StatusOK, config, jsonIndent)
}

// returns the complete file tree
func (rs *RestService) getTree(c echo.Context) error {
	return c.JSONPretty(http.StatusOK, rs.treeManager.Root(), jsonIndent)
}

// describeItem returns the description of a single item of the given type (if found)
func (rs *RestService) describeItem(itemType string) echo.HandlerFunc {
	return func(c echo.Context) error {
		id := c.Param(urlParamID)
		var result any
		switch itemType {
		case TypeSection:
			if s := rs.treeManager.GetSection(id); s != nil {
				result = s
			}
		case TypeDocument:
			if d := rs.treeManager.GetDocument(id); d != nil {
				result = d
			}
		case TypeResource:
			if r := rs.treeManager.GetResource(id); r != nil {
				result = r
			}
		}
		if result == nil {
			return rs.ReturnNotFound(c, id)
		}
		return c.JSONPretty(http.StatusOK, result, jsonIndent)
	}
}

// returns the content of the document with the given id (if found)
func (rs *RestService) getDocumentContent(c echo.Context) error {
	id := c.Param(urlParamID)

	d := rs.treeManager.GetDocument(id)
	if d == nil {
		return rs.ReturnNotFound(c, id)
	}
	if text, ok := rs.sync.CurrentText(d); ok {
		return c.String(http.StatusOK, text)
	}
	content, err := rs.treeManager.ReadDocumentContent(d)
	if err != nil {
		return rs.ReturnError(c, err)
	}
	return c.String(http.StatusOK, content)
}

// bindItemRequest reads a new item request and checks that the parent section exists. It answers
// the request itself on failure, see errResponded.
func (rs *RestService) bindItemRequest(c echo.Context) (*NewItemRequest, error) {
	r := new(NewItemRequest)
	if err := c.Bind(r); err != nil {
		return nil, responded(rs.ReturnError(c, err))
	}
	if rs.treeManager.GetSection(r.Parent) == nil {
		return nil, responded(rs.ReturnNotFound(c, r.Parent))
	}
	return r, nil
}

// creates a new section with the given data
func (rs *RestService) createSection(c echo.Context) error {
	r, err := rs.bindItemRequest(c)
	if err != nil {
		return err
	}
	section, err := rs.treeManager.CreateSection(r.Parent, r.Name)
	if err != nil {
		return rs.ReturnError(c, err)
	}
	return c.JSONPretty(http.StatusOK, section, jsonIndent)
}

// creates a new document with the given data
func (rs *RestService) createDocument(c echo.Context) error {
	r, err := rs.bindItemRequest(c)
	if err != nil {
		return err
	}
	document, err := rs.treeManager.CreateDocument(r.Parent, r.Name)
	if err != nil {
		return rs.ReturnError(c, err)
	}
	rs.commitTree(c, "Create %s", document.Path)
	return c.JSONPretty(http.StatusOK, document, jsonIndent)
}

// renameItem renames an item of the given type, with reference rewriting (see docs/references.md)
func (rs *RestService) renameItem(itemType string) echo.HandlerFunc {
	return func(c echo.Context) error {
		r := new(RenameRequest)
		if err := c.Bind(r); err != nil {
			return rs.ReturnError(c, err)
		}
		return rs.renameViaMove(c, itemType, r.Name)
	}
}

// deleteItem deletes an item of the given type.
// Documents (or sections containing documents) with connected clients are only deleted with ?force=true.
func (rs *RestService) deleteItem(itemType string) echo.HandlerFunc {
	return func(c echo.Context) error {
		id := c.Param(urlParamID)
		path := rs.treeManager.itemPath(id, itemType)
		if path == "" {
			return rs.ReturnNotFound(c, id)
		}

		var success bool
		err := rs.sync.DeleteItem(path, c.QueryParam("force") == "true", func() (err error) {
			success, err = rs.treeManager.DeleteItem(id, itemType)
			return err
		})
		if errors.Is(err, ErrDocumentInUse) {
			return apiError(c, http.StatusConflict, CodeConflict, err.Error()+" (use ?force=true to delete anyway)", nil)
		}
		if err != nil {
			return rs.ReturnError(c, err)
		}
		if !success {
			return rs.ReturnNotFound(c, id)
		}
		rs.commitTree(c, "Delete %s", path)
		return c.NoContent(http.StatusOK)
	}
}

// returns the content of a single resource with the given id (if found)
func (rs *RestService) getResourceContent(c echo.Context) error {
	id := c.Param(urlParamID)

	d := rs.treeManager.GetResource(id)
	if d == nil {
		return rs.ReturnNotFound(c, id)
	}
	return c.File(d.Path)
}

// uploads a new resource file.
// The content is taken from the multipart form field "file", either as a file part (binary safe)
// or, for older clients, as a plain form value.
func (rs *RestService) uploadNewResource(c echo.Context) error {
	parentID := c.Param(urlParamParentID)
	name := c.Param(urlParamName)

	var content io.Reader
	if fileHeader, ferr := c.FormFile("file"); ferr == nil {
		src, err := fileHeader.Open()
		if err != nil {
			return rs.ReturnError(c, err)
		}
		defer func() { _ = src.Close() }()
		content = src
	} else if value := c.FormValue("file"); value != "" {
		content = strings.NewReader(value)
	} else {
		return apiError(c, http.StatusBadRequest, CodeBadRequest, "missing multipart form field 'file'", nil)
	}

	resource, err := rs.treeManager.CreateResource(parentID, name, content)
	if err != nil {
		return rs.ReturnError(c, err)
	}
	rs.commitTree(c, "Upload %s", resource.Path)

	return c.JSONPretty(http.StatusOK, resource, jsonIndent)
}

// ReturnError writes an error response. Known errors are mapped to matching status codes. The
// text of other errors can name server paths, so it is only logged.
func (rs *RestService) ReturnError(c echo.Context, e error) error {
	var httpErr *echo.HTTPError
	switch {
	case errors.Is(e, ErrInvalidName):
		return apiError(c, http.StatusBadRequest, CodeBadRequest, e.Error(), nil)
	case errors.Is(e, ErrAlreadyExists):
		return apiError(c, http.StatusConflict, CodeConflict, e.Error(), nil)
	case errors.Is(e, ErrNotFound):
		return notFoundError(c, e.Error())
	case errors.As(e, &httpErr) && httpErr.Code < http.StatusInternalServerError:
		// c.Bind failed: a malformed body or an unsupported content type
		return apiError(c, httpErr.Code, CodeBadRequest, fmt.Sprint(httpErr.Message), nil)
	}
	log.Printf("%s %s: %v", c.Request().Method, c.Request().URL.Path, e)
	return c.JSONPretty(http.StatusInternalServerError, &ErrorResult{Name: "Unknown Error", Code: CodeInternal, Message: "internal error"}, jsonIndent)
}

// notFoundError writes a 404 response
func notFoundError(c echo.Context, message string) error {
	return c.JSONPretty(http.StatusNotFound, &ErrorResult{Name: "Not found", Code: CodeNotFound, Message: message}, jsonIndent)
}

// return a "not found" message. For an old id of a moved item, details.movedTo names the item now.
func (rs *RestService) ReturnNotFound(c echo.Context, id string) (err error) {
	result := &ErrorResult{
		Name:    "Not found",
		Code:    CodeNotFound,
		Message: "No item with id '" + id + "' found",
	}
	if newTreeLookup(rs.treeManager.Root()).item(id) == nil {
		if item, _, ok := rs.sync.resolveRedirect(id, ""); ok {
			result.Details = map[string]any{"movedTo": item}
		}
	}
	return c.JSONPretty(http.StatusNotFound, result, jsonIndent)
}

// GET /moved/:id/: where the item with an old id is now
func (rs *RestService) getMoved(c echo.Context) error {
	id := c.Param(urlParamID)
	notFound := func() error {
		return apiError(c, http.StatusNotFound, CodeNotFound, "No moved item with id '"+id+"'", nil)
	}
	if newTreeLookup(rs.treeManager.Root()).item(id) != nil {
		return notFound()
	}
	item, entry, ok := rs.sync.resolveRedirect(id, "")
	if !ok {
		return notFound()
	}
	return c.JSONPretty(http.StatusOK, &MovedResponse{OldID: id, Type: item.Type, Item: item, MovedAt: entry.Time}, jsonIndent)
}

// returns the per-character authorship of a document
func (rs *RestService) getDocumentAuthorship(c echo.Context) error {
	id := c.Param(urlParamID)
	d := rs.treeManager.GetDocument(id)
	if d == nil {
		return rs.ReturnNotFound(c, id)
	}
	result, err := rs.sync.Authorship(d)
	if err != nil {
		return rs.ReturnError(c, err)
	}
	return c.JSONPretty(http.StatusOK, result, jsonIndent)
}

// returns the actor map (Automerge actor id -> user)
func (rs *RestService) getActors(c echo.Context) error {
	return c.JSONPretty(http.StatusOK, rs.sync.Actors(), jsonIndent)
}

func (rs *RestService) handleEventsWebsocket(c echo.Context) error {
	return rs.sync.HandleEventsWebsocket(c)
}

func (rs *RestService) handleWebsocket(c echo.Context) error {
	return rs.sync.HandleWebsocket(c, c.Param(urlParamID))
}
