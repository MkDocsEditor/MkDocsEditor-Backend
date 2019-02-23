package frontend

import (
	"MkDocsEditor-Backend/src/backend"
	"MkDocsEditor-Backend/src/config"
	"errors"
	"fmt"
	"github.com/labstack/echo"
	"github.com/labstack/echo/middleware"
	"net/http"
)

const (
	urlParamId      = "id"
	indentationChar = "  "
)

type (
	Result struct {
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
)

func SetupRestService() {
	echoRest := echo.New()
	echoRest.HideBanner = true

	// Root level middleware
	echoRest.Pre(middleware.AddTrailingSlash())

	echoRest.Use(middleware.Secure())

	echoRest.Use(middleware.Logger())
	echoRest.Use(middleware.Recover())

	var allowedOrigins = config.CurrentConfig.Server.CORS.AllowedOrigins
	var allowedMethods = config.CurrentConfig.Server.CORS.AllowedMethods
	if len(allowedOrigins) <= 0 {
		echoRest.Use(middleware.CORS())
	} else {
		echoRest.Use(middleware.CORSWithConfig(middleware.CORSConfig{
			AllowOrigins: allowedOrigins,
			AllowMethods: allowedMethods,
		}))
	}

	// global auth
	var authConf = config.CurrentConfig.Server.BasicAuth
	if authConf.User != "" && authConf.Password != "" {
		echoRest.Use(middleware.BasicAuth(func(username, password string, c echo.Context) (bool, error) {
			if username == authConf.User && password == authConf.Password {
				return true, nil
			}
			return false, nil
		}))
	}

	echoRest.GET("/alive/", isAlive)

	echoRest.GET("/tree/", getTree)

	// Authentication
	// Group level middleware
	groupSections := echoRest.Group("/section")
	groupDocuments := echoRest.Group("/document")
	groupResources := echoRest.Group("/resource")

	groupSections.GET("/", getTree)
	groupSections.GET("/:"+urlParamId+"/", getSectionDescription)
	groupSections.POST("/", createSection)
	groupSections.DELETE("/:"+urlParamId+"/", deleteSection)

	groupDocuments.GET("/:"+urlParamId+"/", getDocumentDescription)
	groupDocuments.GET("/:"+urlParamId+"/ws/", handleNewConnection)
	groupDocuments.GET("/:"+urlParamId+"/content/", getDocumentContent)
	groupDocuments.POST("/", createDocument)
	groupDocuments.DELETE("/:"+urlParamId+"/", deleteDocument)

	groupResources.GET("/:"+urlParamId+"/", getResourceDescription)
	groupResources.GET("/:"+urlParamId+"/content/", getResourceContent)
	groupResources.POST("/", uploadNewResource)
	groupResources.DELETE("/:"+urlParamId+"/", deleteResource)

	var serverConf = config.CurrentConfig.Server
	echoRest.Logger.Fatal(echoRest.Start(fmt.Sprintf("%s:%d", serverConf.Host, serverConf.Port)))
}

// returns an empty "ok" answer
func isAlive(c echo.Context) error {
	return c.NoContent(http.StatusOK)
}

// returns the complete file tree
func getTree(c echo.Context) error {
	return c.JSONPretty(http.StatusOK, backend.DocumentTree, " ")
}

// returns the description of a single section (if found)
func getSectionDescription(c echo.Context) error {
	return getItemDescription(c, backend.TypeSection)
}

// returns the description of a single document (if found)
func getDocumentDescription(c echo.Context) error {
	return getItemDescription(c, backend.TypeDocument)
}

// returns the description of a single document (if found)
func getResourceDescription(c echo.Context) error {
	return getItemDescription(c, backend.TypeResource)
}

func getItemDescription(c echo.Context, itemType string) (err error) {
	id := c.Param(urlParamId)

	var result interface{}
	switch itemType {
	case backend.TypeSection:
		result = backend.GetSection(id)
	case backend.TypeDocument:
		result = backend.GetDocument(id)
	case backend.TypeResource:
		result = backend.GetResource(id)
	default:
		return returnError(c, errors.New("Unknown itemType '"+itemType+"'"))
	}

	if result != nil {
		return c.JSONPretty(http.StatusOK, result, indentationChar)
	} else {
		return returnNotFound(c, id)
	}
}

// returns the content of the document with the given id (if found)
func getDocumentContent(c echo.Context) (err error) {
	id := c.Param(urlParamId)

	d := backend.GetDocument(id)

	if d != nil {
		// TODO: return file again
		//return c.File(d.Path)
		return c.String(http.StatusOK, d.Content)
	} else {
		return returnNotFound(c, id)
	}
}

// creates a new document with the given data
func createSection(c echo.Context) (err error) {
	r := new(NewSectionRequest)
	if err = c.Bind(r); err != nil {
		return returnError(c, err)
	}

	s := backend.GetSection(r.Parent)
	if s == nil {
		return returnNotFound(c, r.Parent)
	}

	if err = backend.CreateSection(s.Path, r.Name); err != nil {
		return returnError(c, err)
	}

	backend.CreateItemTree()
	return c.String(http.StatusOK, "Subsection '"+r.Name+"' created in section '"+s.Name+"'")
}

// creates a new document with the given data
func createDocument(c echo.Context) (err error) {
	r := new(NewDocumentRequest)
	if err = c.Bind(r); err != nil {
		return returnError(c, err)
	}

	s := backend.GetSection(r.Parent)
	if s == nil {
		return returnNotFound(c, r.Parent)
	}
	if err = backend.CreateDocument(r.Parent, r.Name); err != nil {
		return returnError(c, err)
	}

	return c.String(http.StatusOK, "Document '"+r.Name+"' created in section '"+s.Name+"'")
}

// deletes an existing section
func deleteSection(c echo.Context) (err error) {
	return deleteItem(c, backend.TypeSection)
}

// deletes an existing document
func deleteDocument(c echo.Context) (err error) {
	return deleteItem(c, backend.TypeDocument)
}

// deletes an existing resource
func deleteResource(c echo.Context) (err error) {
	return deleteItem(c, backend.TypeResource)
}

// deletes an item by id and itemType
func deleteItem(c echo.Context, itemType string) (err error) {
	id := c.Param(urlParamId)

	success, err := backend.DeleteItem(id, itemType)
	if err != nil {
		return returnError(c, err)
	}

	if !success {
		return returnNotFound(c, id)
	} else {
		backend.CreateItemTree()
		return c.String(http.StatusOK, "Section '"+id+"' deleted")
	}
}

// returns the description of a single resource with the given id (if found)
func GetResourceDescription(c echo.Context) (err error) {
	id := c.Param(urlParamId)
	return c.String(http.StatusOK, "Resource ID: "+id)
}

// returns the content of a single resource with the given id (if found)
func getResourceContent(c echo.Context) (err error) {
	id := c.Param(urlParamId)

	d := backend.GetResource(id)

	if d != nil {
		return c.File(d.Path)
	} else {
		return returnNotFound(c, id)
	}
}

// uploads a new resource file
func uploadNewResource(c echo.Context) (err error) {
	id := c.Param(urlParamId)
	return c.String(http.StatusOK, "Resource ID: "+id)
}

// return the error message of an error
func returnError(c echo.Context, e error) (err error) {
	return c.JSONPretty(http.StatusInternalServerError, &Result{
		Name:    "Unknown Error",
		Message: e.Error(),
	}, indentationChar)
}

// return a "not found" message
func returnNotFound(c echo.Context, id string) (err error) {
	return c.JSONPretty(http.StatusNotFound, &Result{
		Name:    "Not found",
		Message: "No item with id '" + id + "' found",
	}, indentationChar)
}
