// Package api is the REST API and the websocket endpoints of the backend: routes, authentication
// middleware and the HTTP handlers on top of the other packages. See openapi-description.yaml.
package api

import (
	"context"
	"errors"
	"fmt"
	"time"

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
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/collab"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/gitstore"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/push"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/references"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki"
	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"
	"gopkg.in/yaml.v3"
)

const (
	urlParamParentID = "parentID"
	urlParamID       = "id"
	urlParamName     = "name"
	urlParamRevision = "revision"
	jsonIndent       = "  "

	EndpointPathAlive = "/alive/"

	// APIPrefix is the path every route is served under, see docs/sync-protocol.md. Route
	// paths in this package are written without it.
	APIPrefix = "/api"
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
	httpServer     *http.Server
	server         configuration.ServerConfiguration
	allowedOrigins []string
	trustedProxies []*net.IPNet
	authenticator  *auth.Service
	treeManager    *wiki.TreeManager
	sync           *collab.SyncServer
	git            *gitstore.Committer
	push           *push.Notifier
	refs           *references.ReferenceIndex
	// knownRoutes are the route patterns, the only values of the metrics label "route"
	knownRoutes map[string]struct{}
	// thumbnailSlots limits concurrent image decoding
	thumbnailSlots chan struct{}
}

func NewRestService(
	server configuration.ServerConfiguration,
	treeManager *wiki.TreeManager,
	syncServer *collab.SyncServer,
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
		refs:           references.NewReferenceIndex(treeManager, syncServer),
		thumbnailSlots: make(chan struct{}, maxConcurrentThumbnails),
	}
	treeManager.AddTreeChangeListener(func(_, newTree *wiki.Section) { rs.pruneThumbnails(newTree) })
	rs.watchQRCodes()
	rs.echoRest = rs.createRestService()
	rs.httpServer = &http.Server{
		Addr:    net.JoinHostPort(rs.server.Host, strconv.Itoa(rs.server.Port)),
		Handler: rs.echoRest,
		// a client that never finishes its headers does not keep a connection forever
		ReadHeaderTimeout: 30 * time.Second,
	}
	return rs
}

// SetGitCommitter makes tree changes over REST commit to git. Call it before Start.
func (rs *RestService) SetGitCommitter(c *gitstore.Committer) {
	rs.git = c
}

// commitTree commits a tree change of the requesting user, if git is enabled.
// format gets the paths relative to the docs folder.
func (rs *RestService) commitTree(c *echo.Context, format string, absPaths ...string) {
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
	return func(c *echo.Context) error {
		values := c.PathValues()
		decoded := make(echo.PathValues, len(values))
		for i, v := range values {
			d, err := url.PathUnescape(v.Value)
			if err != nil {
				return apiError(c, http.StatusBadRequest, CodeBadRequest, "invalid percent-encoding in the path", nil)
			}
			decoded[i] = echo.PathValue{Name: v.Name, Value: d}
		}
		c.SetPathValues(decoded)
		return next(c)
	}
}

func (rs *RestService) createRestService() *echo.Echo {
	echoRest := echo.New()
	echoRest.IPExtractor = rs.ipExtractor()
	defaultErrorHandler := echo.DefaultHTTPErrorHandler(false)
	echoRest.HTTPErrorHandler = func(c *echo.Context, err error) {
		if errors.Is(err, errResponded) {
			return
		}
		defaultErrorHandler(c, err)
	}

	// Root level middleware
	echoRest.Pre(stripAPIPrefix)
	echoRest.Pre(middleware.AddTrailingSlash())

	echoRest.Use(rs.httpMetrics)
	echoRest.Use(middleware.SecureWithConfig(middleware.SecureConfig{
		XSSProtection:         "0",
		ContentTypeNosniff:    "nosniff",
		XFrameOptions:         "DENY",
		ContentSecurityPolicy: "default-src 'none'; frame-ancestors 'none'",
		ReferrerPolicy:        "no-referrer",
	}))
	echoRest.Use(rs.securityHeaders)

	echoRest.Use(middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
		Skipper:     isAliveCheck,
		LogMethod:   true,
		LogURIPath:  true,
		LogStatus:   true,
		LogLatency:  true,
		HandleError: true,
		LogValuesFunc: func(c *echo.Context, v middleware.RequestLoggerValues) error {
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
	rs.knownRoutes = knownRoutesOf(echoRest)

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
	echoRest.PUT("/account/avatar/", rs.putAccountAvatar)
	echoRest.DELETE("/account/avatar/", rs.deleteAccountAvatar)
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
	echoRest.PUT("/admin/users/:"+urlParamID+"/avatar/", rs.adminPutAvatar)
	echoRest.DELETE("/admin/users/:"+urlParamID+"/avatar/", rs.adminDeleteAvatar)
	echoRest.GET("/admin/users/:"+urlParamID+"/sessions/", rs.adminUserSessions)
	echoRest.POST("/admin/users/:"+urlParamID+"/sessions/revoke/", rs.adminRevokeUserSessions)
	echoRest.POST("/admin/users/:"+urlParamID+"/reset-link/", rs.adminCreateResetLink)
	echoRest.DELETE("/admin/sessions/:"+urlParamID+"/", rs.adminRevokeSession)
	echoRest.GET("/admin/reset-links/", rs.adminListResetLinks)
	echoRest.DELETE("/admin/reset-links/:"+urlParamID+"/", rs.adminRevokeResetLink)
	echoRest.POST("/auth/access-requests/", rs.createAccessRequest)
	echoRest.POST("/auth/access-requests/status/", rs.accessRequestStatus)
	echoRest.GET("/admin/access-requests/", rs.adminAccessRequests)
	echoRest.POST("/admin/access-requests/:"+urlParamID+"/approve/", rs.adminApproveAccessRequest)
	echoRest.POST("/admin/access-requests/:"+urlParamID+"/reject/", rs.adminRejectAccessRequest)
	echoRest.GET("/admin/settings/", rs.adminSettings)
	echoRest.PATCH("/admin/settings/", rs.adminUpdateSettings)
	echoRest.GET("/admin/audit/", rs.adminAudit)
}

// registerContentRoutes adds the endpoints of the wiki content
func (rs *RestService) registerContentRoutes(echoRest *echo.Echo) {
	echoRest.GET("/actors/", rs.getActors)
	echoRest.GET("/avatars/:"+urlParamID+"/:"+urlParamAvatar+"/", rs.getAvatar)
	echoRest.GET("/events/ws/", rs.handleEventsWebsocket)
	echoRest.GET(EndpointPathAlive, rs.isAlive)
	echoRest.GET("/references/broken/", rs.getBrokenReferences)
	echoRest.GET("/references/unused-resources/", rs.getUnusedResources)
	echoRest.GET("/link/", rs.getLink)
	echoRest.POST("/move/", rs.postMove)
	echoRest.GET("/moved/", rs.getMovedList)
	echoRest.GET("/moved/:"+urlParamID+"/", rs.getMoved)
	echoRest.GET("/pins/", rs.getPins)
	echoRest.PUT("/pins/family/order/", rs.changePinOrder(true))
	echoRest.PUT("/pins/personal/order/", rs.changePinOrder(false))
	echoRest.PUT("/pins/family/:"+urlParamID+"/", rs.changePin(true, true))
	echoRest.DELETE("/pins/family/:"+urlParamID+"/", rs.changePin(true, false))
	echoRest.PUT("/pins/personal/:"+urlParamID+"/", rs.changePin(false, true))
	echoRest.DELETE("/pins/personal/:"+urlParamID+"/", rs.changePin(false, false))
	echoRest.GET("/feed/", rs.getFeed)
	echoRest.GET("/push/subscriptions/", rs.getPushSubscriptions)
	echoRest.POST("/push/subscriptions/", rs.postPushSubscription)
	echoRest.DELETE("/push/subscriptions/", rs.deletePushSubscription)
	echoRest.GET("/qr/", rs.getQRCodes)
	echoRest.GET("/qr/:"+urlParamKey+"/", rs.getQRCode)

	groupMkDocs := echoRest.Group("/mkdocs")
	groupSections := echoRest.Group("/section")
	groupDocuments := echoRest.Group("/document")
	groupResources := echoRest.Group("/resource")

	groupMkDocs.GET("/config/", rs.getMkDocsConfig)
	echoRest.GET("/wiki/config/", rs.getWikiConfig)

	groupSections.GET("/", rs.getTree)
	groupSections.GET("/:"+urlParamID+"/", rs.describeItem(wiki.TypeSection))
	groupSections.GET("/:"+urlParamID+"/backlinks/", rs.getSectionBacklinks)
	groupSections.POST("/", rs.createSection)
	groupSections.PUT("/:"+urlParamID+"/", rs.renameItem(wiki.TypeSection))
	groupSections.DELETE("/:"+urlParamID+"/", rs.deleteItem(wiki.TypeSection))

	groupDocuments.GET("/:"+urlParamID+"/", rs.describeItem(wiki.TypeDocument))
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
	groupDocuments.PUT("/:"+urlParamID+"/", rs.renameItem(wiki.TypeDocument))
	groupDocuments.DELETE("/:"+urlParamID+"/", rs.deleteItem(wiki.TypeDocument))

	groupResources.GET("/:"+urlParamID+"/", rs.describeItem(wiki.TypeResource))
	groupResources.GET("/:"+urlParamID+"/content/", rs.getResourceContent)
	groupResources.GET("/:"+urlParamID+"/backlinks/", rs.getResourceBacklinks)
	groupResources.GET("/:"+urlParamID+"/thumbnail/", rs.getResourceThumbnail)
	groupResources.POST("/:"+urlParamParentID+"/:"+urlParamName+"/", rs.uploadNewResource)
	groupResources.PUT("/:"+urlParamID+"/", rs.renameItem(wiki.TypeResource))
	groupResources.DELETE("/:"+urlParamID+"/", rs.deleteItem(wiki.TypeResource))
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
	// resources may have changed while the server was not running
	go rs.pruneThumbnails(rs.treeManager.Root())
	log.Printf("Serving the API at http://%s", rs.httpServer.Addr)
	err := rs.httpServer.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown stops the REST service gracefully
func (rs *RestService) Shutdown(ctx context.Context) {
	if err := rs.httpServer.Shutdown(ctx); err != nil {
		log.Printf("Shutdown: %v", err)
	}
}

// returns an empty "ok" answer
func (rs *RestService) isAlive(c *echo.Context) error {
	return c.NoContent(http.StatusOK)
}

// GET /wiki/config/: the renderer settings of mkdocs.yml without secrets
func (rs *RestService) getWikiConfig(c *echo.Context) error {
	config, err := wiki.ReadWikiConfig(rs.treeManager.ConfigFile())
	if err != nil {
		log.Printf("Unable to read the wiki config: %v", err)
		return apiError(c, http.StatusInternalServerError, CodeInternal, "mkdocs.yml cannot be read", nil)
	}
	return c.JSONPretty(http.StatusOK, config, jsonIndent)
}

func (rs *RestService) getMkDocsConfig(c *echo.Context) error {
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
func (rs *RestService) getTree(c *echo.Context) error {
	return c.JSONPretty(http.StatusOK, rs.treeManager.Root(), jsonIndent)
}

// describeItem returns the description of a single item of the given type (if found)
func (rs *RestService) describeItem(itemType string) echo.HandlerFunc {
	return func(c *echo.Context) error {
		id := c.Param(urlParamID)
		var result any
		switch itemType {
		case wiki.TypeSection:
			if s := rs.treeManager.GetSection(id); s != nil {
				result = s
			}
		case wiki.TypeDocument:
			if d := rs.treeManager.GetDocument(id); d != nil {
				result = d
			}
		case wiki.TypeResource:
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
func (rs *RestService) getDocumentContent(c *echo.Context) error {
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
func (rs *RestService) bindItemRequest(c *echo.Context) (*NewItemRequest, error) {
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
func (rs *RestService) createSection(c *echo.Context) error {
	r, err := rs.bindItemRequest(c)
	if err != nil {
		return err
	}
	section, err := rs.treeManager.CreateSection(r.Parent, r.Name)
	if err != nil {
		return rs.ReturnError(c, err)
	}
	rs.itemCreated(c, wiki.TypeSection, section.Path)
	return c.JSONPretty(http.StatusOK, section, jsonIndent)
}

// creates a new document with the given data
func (rs *RestService) createDocument(c *echo.Context) error {
	r, err := rs.bindItemRequest(c)
	if err != nil {
		return err
	}
	document, err := rs.treeManager.CreateDocument(r.Parent, r.Name)
	if err != nil {
		return rs.ReturnError(c, err)
	}
	rs.commitTree(c, "Create %s", document.Path)
	rs.itemCreated(c, wiki.TypeDocument, document.Path)
	return c.JSONPretty(http.StatusOK, document, jsonIndent)
}

// renameItem renames an item of the given type, with reference rewriting (see docs/references.md)
func (rs *RestService) renameItem(itemType string) echo.HandlerFunc {
	return func(c *echo.Context) error {
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
	return func(c *echo.Context) error {
		id := c.Param(urlParamID)
		path := rs.treeManager.ItemPath(id, itemType)
		if path == "" {
			return rs.ReturnNotFound(c, id)
		}

		var success bool
		err := rs.sync.DeleteItem(path, c.QueryParam("force") == "true", func() (err error) {
			success, err = rs.treeManager.DeleteItem(id, itemType)
			return err
		})
		if errors.Is(err, collab.ErrDocumentInUse) {
			return apiError(c, http.StatusConflict, CodeConflict, err.Error()+" (use ?force=true to delete anyway)", nil)
		}
		if err != nil {
			return rs.ReturnError(c, err)
		}
		if !success {
			return rs.ReturnNotFound(c, id)
		}
		rs.commitTree(c, "Delete %s", path)
		rs.itemDeleted(c, itemType, path)
		return c.NoContent(http.StatusOK)
	}
}

// returns the content of a single resource with the given id (if found)
func (rs *RestService) getResourceContent(c *echo.Context) error {
	id := c.Param(urlParamID)

	d := rs.treeManager.GetResource(id)
	if d == nil {
		return rs.ReturnNotFound(c, id)
	}
	// echo serves files from a fs.FS, which takes no absolute paths
	return c.FileFS(filepath.Base(d.Path), os.DirFS(filepath.Dir(d.Path)))
}

// uploads a new resource file.
// The content is taken from the multipart form field "file", either as a file part (binary safe)
// or, for older clients, as a plain form value.
func (rs *RestService) uploadNewResource(c *echo.Context) error {
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

// bindErrorMessage describes an error of c.Bind. The detail, like the position of a JSON syntax
// error, is in the wrapped error.
func bindErrorMessage(err *echo.HTTPError) string {
	msg := err.Message
	if msg == "" {
		msg = http.StatusText(err.Code)
	}
	if inner := err.Unwrap(); inner != nil {
		msg += ": " + inner.Error()
	}
	return msg
}

// ReturnError writes an error response. Known errors are mapped to matching status codes. The
// text of other errors can name server paths, so it is only logged.
func (rs *RestService) ReturnError(c *echo.Context, e error) error {
	var httpErr *echo.HTTPError
	switch {
	case errors.Is(e, wiki.ErrInvalidName):
		return apiError(c, http.StatusBadRequest, CodeBadRequest, e.Error(), nil)
	case errors.Is(e, wiki.ErrAlreadyExists):
		return apiError(c, http.StatusConflict, CodeConflict, e.Error(), nil)
	case errors.Is(e, wiki.ErrNotFound):
		return notFoundError(c, e.Error())
	case errors.As(e, &httpErr) && httpErr.Code < http.StatusInternalServerError:
		// c.Bind failed: a malformed body or an unsupported content type
		return apiError(c, httpErr.Code, CodeBadRequest, bindErrorMessage(httpErr), nil)
	}
	log.Printf("%s %s: %v", c.Request().Method, c.Request().URL.Path, e)
	return c.JSONPretty(http.StatusInternalServerError, &ErrorResult{Name: "Unknown Error", Code: CodeInternal, Message: "internal error"}, jsonIndent)
}

// notFoundError writes a 404 response
func notFoundError(c *echo.Context, message string) error {
	return c.JSONPretty(http.StatusNotFound, &ErrorResult{Name: "Not found", Code: CodeNotFound, Message: message}, jsonIndent)
}

// return a "not found" message. For an old id of a moved item, details.movedTo names the item now.
func (rs *RestService) ReturnNotFound(c *echo.Context, id string) (err error) {
	result := &ErrorResult{
		Name:    "Not found",
		Code:    CodeNotFound,
		Message: "No item with id '" + id + "' found",
	}
	if references.NewTreeLookup(rs.treeManager.Root()).Item(id) == nil {
		if item, _, ok := rs.sync.ResolveRedirect(id, ""); ok {
			result.Details = map[string]any{"movedTo": item}
		}
	}
	return c.JSONPretty(http.StatusNotFound, result, jsonIndent)
}

// GET /moved/:id/: where the item with an old id is now
func (rs *RestService) getMoved(c *echo.Context) error {
	id := c.Param(urlParamID)
	moved := rs.movedItem(references.NewTreeLookup(rs.treeManager.Root()), id)
	if moved == nil {
		return apiError(c, http.StatusNotFound, CodeNotFound, "No moved item with id '"+id+"'", nil)
	}
	return c.JSONPretty(http.StatusOK, moved, jsonIndent)
}

// GET /moved/?id=…&id=…: GET /moved/:id/ for several ids in one request. Ids that have not
// moved are left out of the answer.
func (rs *RestService) getMovedList(c *echo.Context) error {
	ids := c.QueryParams()["id"]
	if len(ids) > maxMovedLookupIDs {
		return apiError(c, http.StatusBadRequest, CodeBadRequest, "too many ids", map[string]any{
			"reason": "too-many-ids", "maxIds": maxMovedLookupIDs})
	}
	lookup := references.NewTreeLookup(rs.treeManager.Root())
	result := &MovedListResponse{Moved: []*MovedResponse{}}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if moved := rs.movedItem(lookup, id); moved != nil {
			result.Moved = append(result.Moved, moved)
		}
	}
	return c.JSONPretty(http.StatusOK, result, jsonIndent)
}

// movedItem returns where the item with an old id is now, or nil if the id names an existing
// item or has no redirect to an existing item
func (rs *RestService) movedItem(lookup *references.TreeLookup, id string) *MovedResponse {
	if lookup.Item(id) != nil {
		return nil
	}
	item, entry, ok := rs.sync.ResolveRedirectIn(lookup, id, "")
	if !ok {
		return nil
	}
	return &MovedResponse{OldID: id, Type: item.Type, Item: item, MovedAt: entry.Time}
}

// returns the per-character authorship of a document
func (rs *RestService) getDocumentAuthorship(c *echo.Context) error {
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
func (rs *RestService) getActors(c *echo.Context) error {
	return c.JSONPretty(http.StatusOK, rs.sync.Actors(), jsonIndent)
}

func (rs *RestService) handleEventsWebsocket(c *echo.Context) error {
	return rs.sync.HandleEventsWebsocket(c.Response(), c.Request(), UserFromContext(c), sessionIDFromContext(c))
}

func (rs *RestService) handleWebsocket(c *echo.Context) error {
	err := rs.sync.HandleWebsocket(c.Response(), c.Request(), UserFromContext(c), sessionIDFromContext(c), c.Param(urlParamID))
	if errors.Is(err, wiki.ErrNotFound) {
		return echo.ErrNotFound
	}
	return err
}

// stripAPIPrefix serves the routes under APIPrefix: it removes the prefix before routing, and
// answers every other path with 404
func stripAPIPrefix(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		req := c.Request()
		p, ok := cutAPIPrefix(req.URL.Path)
		if !ok {
			c.Response().Header().Set(echo.HeaderCacheControl, "no-store")
			return apiError(c, http.StatusNotFound, CodeNotFound, "the API is under "+APIPrefix+"/", nil)
		}
		req.URL.Path = p
		if req.URL.RawPath != "" {
			req.URL.RawPath, _ = cutAPIPrefix(req.URL.RawPath)
		}
		return next(c)
	}
}

func cutAPIPrefix(p string) (string, bool) {
	rest, ok := strings.CutPrefix(p, APIPrefix)
	if !ok || (rest != "" && !strings.HasPrefix(rest, "/")) {
		return "", false
	}
	if rest == "" {
		rest = "/"
	}
	return rest, true
}
