package backend

import (
	"context"
	"errors"
	"fmt"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"gopkg.in/yaml.v3"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const (
	urlParamParentId = "parentId"
	urlParamId       = "id"
	urlParamName     = "name"
	urlParamRevision = "revision"
	indentationChar  = "  "

	EndpointPathAlive = "/alive/"
)

type (
	ErrorResult struct {
		Name    string         `json:"name"`
		Code    string         `json:"code,omitempty"`
		Message string         `json:"message"`
		Details map[string]any `json:"details,omitempty"`
	}

	NewSectionRequest struct {
		Parent string `json:"parent" xml:"parent" form:"parent" query:"parent" validate:"required"`
		Name   string `json:"name" xml:"name" form:"name" query:"name" validate:"required"`
	}

	NewDocumentRequest struct {
		Parent string `json:"parent" xml:"parent" form:"parent" query:"parent" validate:"required"`
		Name   string `json:"name" xml:"name" form:"name" query:"name" validate:"required"`
	}

	RenameSectionRequest struct {
		Name string `json:"name" xml:"name" form:"name" query:"name" validate:"required"`
	}

	RenameDocumentRequest struct {
		Name string `json:"name" xml:"name" form:"name" query:"name" validate:"required"`
	}

	RenameResourceRequest struct {
		Name string `json:"name" xml:"name" form:"name" query:"name" validate:"required"`
	}
)

type RestService struct {
	echoRest       *echo.Echo
	allowedOrigins []string
	trustedProxies []*net.IPNet
	authenticator  *auth.Service
	treeManager    *TreeManager
	sync           *SyncServer
	git            *GitCommitter
}

func NewRestService(
	treeManager *TreeManager,
	syncServer *SyncServer,
	authenticator *auth.Service,
) *RestService {
	trusted, err := ParseTrustedProxies(configuration.CurrentConfig.Server.TrustedProxies)
	if err != nil {
		log.Printf("Ignoring server.trustedProxies: %v", err)
	}
	rs := &RestService{
		treeManager:    treeManager,
		sync:           syncServer,
		authenticator:  authenticator,
		allowedOrigins: configuration.CurrentConfig.Server.CORS.AllowedOrigins,
		trustedProxies: trusted,
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
		if err != nil || strings.HasPrefix(rel, "..") {
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

	echoRest.Use(middleware.CORSWithConfig(corsConfig(configuration.CurrentConfig.Server.CORS)))

	// global auth
	echoRest.Use(rs.authMiddleware)
	echoRest.Use(unescapePathParams)

	echoRest.GET("/auth/status/", rs.authStatus)
	echoRest.POST("/auth/login/", rs.login)
	echoRest.POST("/auth/logout/", rs.logout)
	echoRest.POST("/auth/link/inspect/", rs.inspectLink)
	echoRest.POST("/auth/link/redeem/", rs.redeemLink)
	echoRest.GET(EndpointPathWhoAmI, rs.whoAmI)
	echoRest.POST(EndpointPathWsTicket, rs.issueWsTicket)

	echoRest.GET("/account/sessions/", rs.accountSessions)
	echoRest.DELETE("/account/sessions/:"+urlParamId+"/", rs.revokeAccountSession)
	echoRest.POST("/account/sessions/revoke-others/", rs.revokeOtherSessions)

	echoRest.GET("/admin/invites/", rs.listInvites)
	echoRest.POST("/admin/invites/", rs.createInvite)
	echoRest.DELETE("/admin/invites/:"+urlParamId+"/", rs.revokeInvite)

	echoRest.GET("/actors/", rs.getActors)
	echoRest.GET("/events/ws/", rs.handleEventsWebsocket)
	echoRest.GET(EndpointPathAlive, rs.isAlive)

	// Authentication
	// Group level middleware
	groupMkDocs := echoRest.Group("/mkdocs")
	groupSections := echoRest.Group("/section")
	groupDocuments := echoRest.Group("/document")
	groupResources := echoRest.Group("/resource")

	groupMkDocs.GET("/config/", rs.getMkDocsConfig)

	groupSections.GET("/", rs.getTree)
	groupSections.GET("/:"+urlParamId+"/", rs.getSectionDescription)
	groupSections.POST("/", rs.createSection)
	groupSections.PUT("/:"+urlParamId+"/", rs.renameSection)
	groupSections.DELETE("/:"+urlParamId+"/", rs.deleteSection)

	groupDocuments.GET("/:"+urlParamId+"/", rs.getDocumentDescription)
	groupDocuments.GET("/:"+urlParamId+"/ws/", rs.handleWebsocket)
	groupDocuments.GET("/:"+urlParamId+"/content/", rs.getDocumentContent)
	groupDocuments.GET("/:"+urlParamId+"/authorship/", rs.getDocumentAuthorship)
	groupDocuments.GET("/:"+urlParamId+"/history/", rs.getDocumentHistory)
	groupDocuments.GET("/:"+urlParamId+"/history/diff/", rs.getDocumentRevisionDiff)
	groupDocuments.GET("/:"+urlParamId+"/history/:"+urlParamRevision+"/content/", rs.getDocumentRevisionContent)
	groupDocuments.POST("/:"+urlParamId+"/history/:"+urlParamRevision+"/restore/", rs.restoreDocumentRevision)
	groupDocuments.POST("/", rs.createDocument)
	groupDocuments.PUT("/:"+urlParamId+"/", rs.renameDocument)
	groupDocuments.DELETE("/:"+urlParamId+"/", rs.deleteDocument)

	groupResources.GET("/:"+urlParamId+"/", rs.getResourceDescription)
	groupResources.GET("/:"+urlParamId+"/content/", rs.getResourceContent)
	groupResources.POST("/:"+urlParamParentId+"/:"+urlParamName+"/", rs.uploadNewResource)
	groupResources.PUT("/:"+urlParamId+"/", rs.renameResource)
	groupResources.DELETE("/:"+urlParamId+"/", rs.deleteResource)

	return echoRest
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

// Start the REST service. It returns after Shutdown.
func (rs *RestService) Start() {
	var serverConf = configuration.CurrentConfig.Server
	err := rs.echoRest.Start(fmt.Sprintf("%s:%d", serverConf.Host, serverConf.Port))
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		rs.echoRest.Logger.Fatal(err)
	}
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
	mkDocsConfigFileContent, err := os.ReadFile(configuration.CurrentConfig.MkDocs.ConfigFile)
	if err != nil {
		return rs.ReturnError(c, err)
	}

	var config map[string]interface{}
	// Unmarshal the YAML data into the map
	err = yaml.Unmarshal(mkDocsConfigFileContent, &config)
	if err != nil {
		return rs.ReturnError(c, err)
	}
	return c.JSONPretty(http.StatusOK, config, indentationChar)
}

// returns the complete file tree
func (rs *RestService) getTree(c echo.Context) error {
	return c.JSONPretty(http.StatusOK, rs.treeManager.Root(), indentationChar)
}

// returns the description of a single section (if found)
func (rs *RestService) getSectionDescription(c echo.Context) error {
	return rs.getItemDescription(c, TypeSection)
}

// returns the description of a single document (if found)
func (rs *RestService) getDocumentDescription(c echo.Context) error {
	return rs.getItemDescription(c, TypeDocument)
}

// returns the description of a single document (if found)
func (rs *RestService) getResourceDescription(c echo.Context) error {
	return rs.getItemDescription(c, TypeResource)
}

func (rs *RestService) getItemDescription(c echo.Context, itemType string) (err error) {
	id := c.Param(urlParamId)

	var result interface{}
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
	default:
		return rs.ReturnError(c, errors.New("Unknown itemType '"+itemType+"'"))
	}

	if result == nil {
		return rs.ReturnNotFound(c, id)
	}
	return c.JSONPretty(http.StatusOK, result, indentationChar)
}

// returns the content of the document with the given id (if found)
func (rs *RestService) getDocumentContent(c echo.Context) (err error) {
	id := c.Param(urlParamId)

	d := rs.treeManager.GetDocument(id)
	if d == nil {
		return rs.ReturnNotFound(c, id)
	}
	if rs.sync != nil {
		if text, ok := rs.sync.CurrentText(d); ok {
			return c.String(http.StatusOK, text)
		}
	}
	content, err := rs.treeManager.ReadDocumentContent(d)
	if err != nil {
		return rs.ReturnError(c, err)
	}
	return c.String(http.StatusOK, content)
}

// creates a new document with the given data
func (rs *RestService) createSection(c echo.Context) (err error) {
	r := new(NewSectionRequest)
	if err = c.Bind(r); err != nil {
		return rs.ReturnError(c, err)
	}

	if rs.treeManager.GetSection(r.Parent) == nil {
		return rs.ReturnNotFound(c, r.Parent)
	}

	section, err := rs.treeManager.CreateSection(r.Parent, r.Name)
	if err != nil {
		return rs.ReturnError(c, err)
	}

	return c.JSONPretty(http.StatusOK, section, indentationChar)
}

// renames an existing section
func (rs *RestService) renameSection(c echo.Context) (err error) {
	id := c.Param(urlParamId)
	r := new(RenameSectionRequest)
	if err = c.Bind(r); err != nil {
		return rs.ReturnError(c, err)
	}

	s := rs.treeManager.GetSection(id)
	if s == nil {
		return rs.ReturnNotFound(c, id)
	}

	var section *Section
	newPath := filepath.Join(filepath.Dir(s.Path), r.Name)
	err = rs.moveItem(s.Path, newPath, func() (err error) {
		section, err = rs.treeManager.RenameSection(s, r.Name)
		return err
	})
	if err != nil {
		return rs.ReturnError(c, err)
	}
	rs.commitTree(c, "Rename %s to %s", s.Path, newPath)

	return c.JSONPretty(http.StatusOK, section, indentationChar)
}

// creates a new document with the given data
func (rs *RestService) createDocument(c echo.Context) (err error) {
	r := new(NewDocumentRequest)
	if err = c.Bind(r); err != nil {
		return rs.ReturnError(c, err)
	}

	s := rs.treeManager.GetSection(r.Parent)
	if s == nil {
		return rs.ReturnNotFound(c, r.Parent)
	}
	document, err := rs.treeManager.CreateDocument(r.Parent, r.Name)
	if err != nil {
		return rs.ReturnError(c, err)
	}
	rs.commitTree(c, "Create %s", document.Path)

	return c.JSONPretty(http.StatusOK, document, indentationChar)
}

func (rs *RestService) renameDocument(c echo.Context) (err error) {
	id := c.Param(urlParamId)
	r := new(RenameDocumentRequest)
	if err = c.Bind(r); err != nil {
		return rs.ReturnError(c, err)
	}

	d := rs.treeManager.GetDocument(id)
	if d == nil {
		return rs.ReturnNotFound(c, id)
	}

	var document *Document
	newPath := filepath.Join(filepath.Dir(d.Path), r.Name+markdownFileExtension)
	err = rs.moveItem(d.Path, newPath, func() (err error) {
		document, err = rs.treeManager.RenameDocument(d, r.Name)
		return err
	})
	if err != nil {
		return rs.ReturnError(c, err)
	}
	rs.commitTree(c, "Rename %s to %s", d.Path, newPath)
	return c.JSONPretty(http.StatusOK, document, indentationChar)
}

func (rs *RestService) renameResource(c echo.Context) (err error) {
	id := c.Param(urlParamId)
	r := new(RenameResourceRequest)
	if err = c.Bind(r); err != nil {
		return rs.ReturnError(c, err)
	}

	d := rs.treeManager.GetResource(id)
	if d == nil {
		return rs.ReturnNotFound(c, id)
	}

	var resource *Resource
	newPath := filepath.Join(filepath.Dir(d.Path), r.Name)
	err = rs.moveItem(d.Path, newPath, func() (err error) {
		resource, err = rs.treeManager.RenameResource(d, r.Name)
		return err
	})
	if err != nil {
		return rs.ReturnError(c, err)
	}
	rs.commitTree(c, "Rename %s to %s", d.Path, newPath)
	return c.JSONPretty(http.StatusOK, resource, indentationChar)
}

// deletes an existing section
func (rs *RestService) deleteSection(c echo.Context) (err error) {
	return rs.deleteItem(c, TypeSection)
}

// deletes an existing document
func (rs *RestService) deleteDocument(c echo.Context) (err error) {
	return rs.deleteItem(c, TypeDocument)
}

// deletes an existing resource
func (rs *RestService) deleteResource(c echo.Context) (err error) {
	return rs.deleteItem(c, TypeResource)
}

// moves the Automerge state of affected documents along with a rename
func (rs *RestService) moveItem(oldPath string, newPath string, rename func() error) error {
	if rs.sync == nil {
		return rename()
	}
	return rs.sync.MoveItem(oldPath, newPath, rename)
}

// deletes an item by id and itemType.
// Documents (or sections containing documents) with connected clients are only deleted with ?force=true.
func (rs *RestService) deleteItem(c echo.Context, itemType string) (err error) {
	id := c.Param(urlParamId)

	var path string
	switch itemType {
	case TypeSection:
		if s := rs.treeManager.GetSection(id); s != nil {
			path = s.Path
		}
	case TypeDocument:
		if d := rs.treeManager.GetDocument(id); d != nil {
			path = d.Path
		}
	case TypeResource:
		if r := rs.treeManager.GetResource(id); r != nil {
			path = r.Path
		}
	}
	if path == "" {
		return rs.ReturnNotFound(c, id)
	}

	var success bool
	remove := func() (err error) {
		success, err = rs.treeManager.DeleteItem(id, itemType)
		return err
	}
	if rs.sync != nil {
		err = rs.sync.DeleteItem(path, c.QueryParam("force") == "true", remove)
	} else {
		err = remove()
	}
	if errors.Is(err, ErrDocumentInUse) {
		return c.JSONPretty(http.StatusConflict, &ErrorResult{
			Name:    "Conflict",
			Code:    CodeConflict,
			Message: err.Error() + " (use ?force=true to delete anyway)",
		}, indentationChar)
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

// returns the content of a single resource with the given id (if found)
func (rs *RestService) getResourceContent(c echo.Context) (err error) {
	id := c.Param(urlParamId)

	d := rs.treeManager.GetResource(id)
	if d == nil {
		return rs.ReturnNotFound(c, id)
	}
	return c.File(d.Path)
}

// uploads a new resource file.
// The content is taken from the multipart form field "file", either as a file part (binary safe)
// or, for older clients, as a plain form value.
func (rs *RestService) uploadNewResource(c echo.Context) (err error) {
	parentId := c.Param(urlParamParentId)
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
		return c.JSONPretty(http.StatusBadRequest, &ErrorResult{
			Name:    "Bad Request",
			Code:    CodeBadRequest,
			Message: "missing multipart form field 'file'",
		}, indentationChar)
	}

	resource, err := rs.treeManager.CreateResource(parentId, name, content)
	if err != nil {
		return rs.ReturnError(c, err)
	}
	rs.commitTree(c, "Upload %s", resource.Path)

	return c.JSONPretty(http.StatusOK, resource, indentationChar)
}

// ReturnError writes an error response. Known errors are mapped to matching status codes.
func (rs *RestService) ReturnError(c echo.Context, e error) (err error) {
	status, name := http.StatusInternalServerError, "Unknown Error"
	switch {
	case errors.Is(e, ErrInvalidName):
		status, name = http.StatusBadRequest, "Bad Request"
	case errors.Is(e, ErrAlreadyExists):
		status, name = http.StatusConflict, "Conflict"
	case errors.Is(e, ErrNotFound):
		status, name = http.StatusNotFound, "Not found"
	}
	code := map[int]string{http.StatusBadRequest: CodeBadRequest, http.StatusConflict: CodeConflict, http.StatusNotFound: CodeNotFound}[status]
	if code == "" {
		code = CodeInternal
	}
	return c.JSONPretty(status, &ErrorResult{
		Name:    name,
		Code:    code,
		Message: e.Error(),
	}, indentationChar)
}

// return a "not found" message
func (rs *RestService) ReturnNotFound(c echo.Context, id string) (err error) {
	return c.JSONPretty(http.StatusNotFound, &ErrorResult{
		Name:    "Not found",
		Code:    CodeNotFound,
		Message: "No item with id '" + id + "' found",
	}, indentationChar)
}

// returns the per-character authorship of a document
func (rs *RestService) getDocumentAuthorship(c echo.Context) error {
	id := c.Param(urlParamId)
	d := rs.treeManager.GetDocument(id)
	if d == nil {
		return rs.ReturnNotFound(c, id)
	}
	if rs.sync == nil {
		return echo.ErrNotFound
	}
	result, err := rs.sync.Authorship(d)
	if err != nil {
		return rs.ReturnError(c, err)
	}
	return c.JSONPretty(http.StatusOK, result, indentationChar)
}

// returns the actor map (Automerge actor id -> user)
func (rs *RestService) getActors(c echo.Context) error {
	if rs.sync == nil {
		return c.JSONPretty(http.StatusOK, []ActorInfo{}, indentationChar)
	}
	return c.JSONPretty(http.StatusOK, rs.sync.Actors(), indentationChar)
}

func (rs *RestService) handleEventsWebsocket(c echo.Context) error {
	if rs.sync == nil {
		return echo.ErrNotFound
	}
	return rs.sync.HandleEventsWebsocket(c)
}

func (rs *RestService) handleWebsocket(c echo.Context) error {
	if rs.sync == nil {
		return echo.ErrNotFound
	}
	return rs.sync.HandleWebsocket(c, c.Param(urlParamId))
}
