package backend

import (
	"errors"
	"fmt"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"gopkg.in/yaml.v3"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
)

const (
	urlParamParentId = "parentId"
	urlParamId       = "id"
	urlParamName     = "name"
	indentationChar  = "  "

	EndpointPathAlive = "/alive/"
)

type (
	ErrorResult struct {
		Name    string `json:"name" xml:"name" form:"name" query:"name"`
		Message string `json:"message" xml:"message" form:"message" query:"message"`
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
	echoRest                   *echo.Echo
	treeManager                *TreeManager
	syncManager                SyncManager
	websocketConnectionManager *WebsocketConnectionManager
}

func NewRestService(
	treeManager *TreeManager,
	syncManager SyncManager,
) *RestService {
	rs := &RestService{
		treeManager: treeManager,
		syncManager: syncManager,
	}
	rs.echoRest = rs.createRestService()
	return rs
}

func (rs *RestService) createRestService() *echo.Echo {
	echoRest := echo.New()
	echoRest.HideBanner = true

	// Root level middleware
	echoRest.Pre(middleware.AddTrailingSlash())

	echoRest.Use(middleware.Secure())

	echoRest.Use(middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
		LogMethod:   true,
		LogURIPath:  true,
		LogStatus:   true,
		LogLatency:  true,
		LogError:    true,
		HandleError: true,
		LogValuesFunc: func(c echo.Context, v middleware.RequestLoggerValues) error {
			if v.Error != nil {
				log.Printf("%s %s %d %v error=%v", v.Method, v.URIPath, v.Status, v.Latency, v.Error)
			} else {
				log.Printf("%s %s %d %v", v.Method, v.URIPath, v.Status, v.Latency)
			}
			return nil
		},
	}))
	echoRest.Use(middleware.Recover())

	var allowedOrigins = configuration.CurrentConfig.Server.CORS.AllowedOrigins
	var allowedMethods = configuration.CurrentConfig.Server.CORS.AllowedMethods
	if len(allowedOrigins) <= 0 {
		echoRest.Use(middleware.CORS())
	} else {
		echoRest.Use(middleware.CORSWithConfig(middleware.CORSConfig{
			AllowOrigins: allowedOrigins,
			AllowMethods: allowedMethods,
		}))
	}

	// global auth
	var authConf = configuration.CurrentConfig.Server.BasicAuth
	if authConf.User != "" && authConf.Password != "" {
		basicAuthConfig := middleware.BasicAuthConfig{
			Skipper: func(context echo.Context) bool {
				return context.Path() == EndpointPathAlive
			},
			Validator: func(username string, password string, context echo.Context) (b bool, err error) {
				if username == authConf.User && password == authConf.Password {
					return true, nil
				}
				return false, nil
			},
			Realm: "Restricted",
		}
		echoRest.Use(middleware.BasicAuthWithConfig(basicAuthConfig))
	}

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
	groupDocuments.GET("/:"+urlParamId+"/ws/", rs.handleNewConnection)
	groupDocuments.GET("/:"+urlParamId+"/content/", rs.getDocumentContent)
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

// Start the REST service
func (rs *RestService) Start() {
	var serverConf = configuration.CurrentConfig.Server
	rs.echoRest.Logger.Fatal(rs.echoRest.Start(fmt.Sprintf("%s:%d", serverConf.Host, serverConf.Port)))
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

	section, err := rs.treeManager.RenameSection(s, r.Name)
	if err != nil {
		return rs.ReturnError(c, err)
	}

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

	document, err := rs.treeManager.RenameDocument(d, r.Name)
	if err != nil {
		return rs.ReturnError(c, err)
	}
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

	resource, err := rs.treeManager.RenameResource(d, r.Name)
	if err != nil {
		return rs.ReturnError(c, err)
	}
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

// deletes an item by id and itemType
func (rs *RestService) deleteItem(c echo.Context, itemType string) (err error) {
	id := c.Param(urlParamId)

	switch itemType {
	case TypeSection:
		if s := rs.treeManager.GetSection(id); s != nil && rs.syncManager != nil {
			if err = rs.syncManager.IsItemBeingEditedRecursive(s); err != nil {
				return c.JSONPretty(http.StatusConflict, &ErrorResult{
					Name:    "Conflict",
					Message: err.Error(),
				}, indentationChar)
			}
		}
	case TypeDocument:
		if rs.websocketConnectionManager != nil && rs.websocketConnectionManager.IsClientConnected(id) {
			return c.JSONPretty(http.StatusConflict, &ErrorResult{
				Name:    "Conflict",
				Message: "There are still clients connected to the document",
			}, indentationChar)
		}
	}

	success, err := rs.treeManager.DeleteItem(id, itemType)
	if err != nil {
		return rs.ReturnError(c, err)
	}

	if !success {
		return rs.ReturnNotFound(c, id)
	}
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
			Message: "missing multipart form field 'file'",
		}, indentationChar)
	}

	resource, err := rs.treeManager.CreateResource(parentId, name, content)
	if err != nil {
		return rs.ReturnError(c, err)
	}

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
	return c.JSONPretty(status, &ErrorResult{
		Name:    name,
		Message: e.Error(),
	}, indentationChar)
}

// return a "not found" message
func (rs *RestService) ReturnNotFound(c echo.Context, id string) (err error) {
	return c.JSONPretty(http.StatusNotFound, &ErrorResult{
		Name:    "Not found",
		Message: "No item with id '" + id + "' found",
	}, indentationChar)
}

func (rs *RestService) RegisterWebsocketHandler(websocketConnectionManager *WebsocketConnectionManager) {
	rs.websocketConnectionManager = websocketConnectionManager
	rs.echoRest.GET("/:"+urlParamId+"/ws/", rs.handleNewConnection)
}

func (rs *RestService) handleNewConnection(c echo.Context) (err error) {
	documentId := c.Param(urlParamId)
	return rs.websocketConnectionManager.HandleNewConnection(c, documentId)
}
