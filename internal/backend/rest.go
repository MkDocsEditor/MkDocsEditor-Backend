package backend

import (
	"errors"
	"fmt"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"gopkg.in/yaml.v3"
	"net/http"
	"os"
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
	echoRest    *echo.Echo
	treeManager *TreeManager
	syncManager *SyncManager
}

func NewRestService(
	treeManager *TreeManager,
	syncManager *SyncManager,
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

	echoRest.Use(middleware.Logger())
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

	echoRest.GET("/tree/", rs.getTree)

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

func (rs *RestService) IsClientConnected(documentId string) bool {
	return rs.syncManager.IsClientConnected(documentId)
}

// returns an empty "ok" answer
func (rs *RestService) isAlive(c echo.Context) error {
	return c.NoContent(http.StatusOK)
}

func (rs *RestService) getMkDocsConfig(c echo.Context) error {
	mkDocsConfigFileContent, err := os.ReadFile(configuration.CurrentConfig.MkDocs.ConfigFile)

	var config map[string]interface{}
	// Unmarshal the YAML data into the map
	err = yaml.Unmarshal(mkDocsConfigFileContent, &config)
	if err != nil {
		return rs.returnError(c, err)
	}
	return c.JSONPretty(http.StatusOK, config, indentationChar)
}

// returns the complete file tree
func (rs *RestService) getTree(c echo.Context) error {
	return c.JSONPretty(http.StatusOK, rs.treeManager.DocumentTree, " ")
}

// returns the description of a single section (if found)
func (rs *RestService) getSectionDescription(c echo.Context) error {
	return rs.getItemDescription(c, TypeSection)
}

// returns the description of a single document (if found)
func (rs *RestService) getDocumentDescription(c echo.Context) error {
	return rs.getItemDescription(c, TypeDocument)
}

func (rs *RestService) handleNewConnection(c echo.Context) (err error) {
	documentId := c.Param(urlParamId)
	err = rs.syncManager.websocketConnectionManager.handleNewConnection(c, documentId)
	if err != nil {
		if errors.Is(err, echo.ErrNotFound) {
			return rs.returnNotFound(c, documentId)
		} else {
			return rs.returnError(c, err)
		}
	}
	return nil
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
		result = rs.treeManager.GetSection(id)
	case TypeDocument:
		result = rs.treeManager.GetDocument(id)
	case TypeResource:
		result = rs.treeManager.GetResource(id)
	default:
		return rs.returnError(c, errors.New("Unknown itemType '"+itemType+"'"))
	}

	if result != nil {
		return c.JSONPretty(http.StatusOK, result, indentationChar)
	} else {
		return rs.returnNotFound(c, id)
	}
}

// returns the content of the document with the given id (if found)
func (rs *RestService) getDocumentContent(c echo.Context) (err error) {
	id := c.Param(urlParamId)

	d := rs.treeManager.GetDocument(id)

	if d != nil {
		return c.String(http.StatusOK, d.Content)
	} else {
		return rs.returnNotFound(c, id)
	}
}

// creates a new document with the given data
func (rs *RestService) createSection(c echo.Context) (err error) {
	r := new(NewSectionRequest)
	if err = c.Bind(r); err != nil {
		return rs.returnError(c, err)
	}

	s := rs.treeManager.GetSection(r.Parent)
	if s == nil {
		return rs.returnNotFound(c, r.Parent)
	}

	section, err := rs.treeManager.CreateSection(s, r.Name)
	if err != nil {
		return rs.returnError(c, err)
	}

	return c.JSONPretty(http.StatusOK, section, " ")
}

// renames an existing section
func (rs *RestService) renameSection(c echo.Context) (err error) {
	id := c.Param(urlParamId)
	r := new(RenameSectionRequest)
	if err = c.Bind(r); err != nil {
		return rs.returnError(c, err)
	}

	s := rs.treeManager.GetSection(id)
	if s == nil {
		return rs.returnNotFound(c, id)
	}

	section, err := rs.treeManager.RenameSection(s, r.Name)
	if err != nil {
		return rs.returnError(c, err)
	}

	return c.JSONPretty(http.StatusOK, section, " ")
}

// creates a new document with the given data
func (rs *RestService) createDocument(c echo.Context) (err error) {
	r := new(NewDocumentRequest)
	if err = c.Bind(r); err != nil {
		return rs.returnError(c, err)
	}

	s := rs.treeManager.GetSection(r.Parent)
	if s == nil {
		return rs.returnNotFound(c, r.Parent)
	}
	document, err := rs.treeManager.CreateDocument(r.Parent, r.Name)
	if err != nil {
		return rs.returnError(c, err)
	}

	return c.JSONPretty(http.StatusOK, document, " ")
}

func (rs *RestService) renameDocument(c echo.Context) (err error) {
	id := c.Param(urlParamId)
	r := new(RenameDocumentRequest)
	if err = c.Bind(r); err != nil {
		return rs.returnError(c, err)
	}

	d := rs.treeManager.GetDocument(id)
	if d == nil {
		return rs.returnNotFound(c, id)
	}

	document, err := rs.treeManager.RenameDocument(d, r.Name)
	if err != nil {
		return rs.returnError(c, err)
	}
	return c.JSONPretty(http.StatusOK, document, " ")
}

func (rs *RestService) renameResource(c echo.Context) (err error) {
	id := c.Param(urlParamId)
	r := new(RenameResourceRequest)
	if err = c.Bind(r); err != nil {
		return rs.returnError(c, err)
	}

	d := rs.treeManager.GetResource(id)
	if d == nil {
		return rs.returnNotFound(c, id)
	}

	resource, err := rs.treeManager.RenameResource(d, r.Name)
	if err != nil {
		return rs.returnError(c, err)
	}
	return c.JSONPretty(http.StatusOK, resource, " ")
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
		err = rs.syncManager.IsItemBeingEditedRecursive(rs.treeManager.GetSection(id))
		if err != nil {
			return rs.returnError(c, err)
		}
	case TypeDocument:
		if rs.IsClientConnected(id) {
			return c.JSONPretty(http.StatusConflict, &ErrorResult{
				Name:    "Conflict",
				Message: "There are still clients connected to the document",
			}, indentationChar)
		}
	}

	success, err := rs.treeManager.DeleteItem(id, itemType)
	if err != nil {
		return rs.returnError(c, err)
	}

	if !success {
		return rs.returnNotFound(c, id)
	} else {
		rs.treeManager.CreateItemTree()
		return c.NoContent(http.StatusOK)
	}
}

// GetResourceDescription returns the description of a single resource with the given id (if found)
func (rs *RestService) GetResourceDescription(c echo.Context) (err error) {
	id := c.Param(urlParamId)
	return c.String(http.StatusOK, "Resource ID: "+id)
}

// returns the content of a single resource with the given id (if found)
func (rs *RestService) getResourceContent(c echo.Context) (err error) {
	id := c.Param(urlParamId)

	d := rs.treeManager.GetResource(id)

	if d != nil {
		return c.File(d.Path)
	} else {
		return rs.returnNotFound(c, id)
	}
}

// uploads a new resource file
func (rs *RestService) uploadNewResource(c echo.Context) (err error) {
	parentId := c.Param(urlParamParentId)
	name := c.Param(urlParamName)

	content := []byte{}
	_, err = c.Request().Body.Read(content)
	if err != nil {
		return err
	}

	fileContent := c.FormValue("file")

	//file, err := c.FormFile("file")
	//if err != nil {
	//	return err
	//}
	//src, err := file.Open()
	//if err != nil {
	//	return err
	//}
	//
	//resource, err := CreateResource(parentId, name, src)

	resource, err := rs.treeManager.CreateResource(parentId, name, fileContent)
	if err != nil {
		return rs.returnError(c, err)
	}

	return c.JSONPretty(http.StatusOK, resource, indentationChar)
}

// return the error message of an error
func (rs *RestService) returnError(c echo.Context, e error) (err error) {
	return c.JSONPretty(http.StatusInternalServerError, &ErrorResult{
		Name:    "Unknown Error",
		Message: e.Error(),
	}, indentationChar)
}

// return a "not found" message
func (rs *RestService) returnNotFound(c echo.Context, id string) (err error) {
	return c.JSONPretty(http.StatusNotFound, &ErrorResult{
		Name:    "Not found",
		Message: "No item with id '" + id + "' found",
	}, indentationChar)
}
