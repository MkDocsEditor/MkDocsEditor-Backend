package main

import (
	"github.com/labstack/echo"
	"net/http"
	"mkdocsrest/config"
	"github.com/labstack/echo/middleware"
	"fmt"
	"mkdocsrest/backend"
)

const paramId = "id"

type NewDocumentRequest struct {
	Name    string `json:"name" xml:"name" form:"name" query:"name"`
	Content string `json:"content" xml:"content" form:"content" query:"content"`
}

type UpdateDocumentRequest struct {
	Name    string `json:"name" xml:"name" form:"name" query:"name"`
	Content string `json:"content" xml:"content" form:"content" query:"content"`
}

type Error struct {
	Message string `json:"message" xml:"message" form:"message" query:"message"`
}

// main entry point
func main() {
	config.Setup()

	backend.SetupCache()
	backend.UpdateCache()

	backend.CreateDocumentTree()

	setupRestService()
}

func setupRestService() {
	echoRest := echo.New()

	// Root level middleware
	echoRest.Use(middleware.Logger())
	echoRest.Use(middleware.Recover())

	// global auth
	var authConf = config.CurrentConfig.Server.Auth
	echoRest.Use(middleware.BasicAuth(func(username, password string, c echo.Context) (bool, error) {
		if username == authConf.User && password == authConf.Password {
			return true, nil
		}
		return false, nil
	}))

	// Authentication
	// Group level middleware
	groupDocuments := echoRest.Group("/documents")
	groupDocuments.GET("/", GetDocumentDescriptions)
	groupDocuments.GET("/:id", GetDocumentDescription)
	groupDocuments.GET("/:id/content", GetDocumentContent)
	groupDocuments.POST("/:id", UpdateDocument)
	groupDocuments.PUT("/", CreateDocument)
	groupDocuments.DELETE("/:id", DeleteDocument)

	groupResources := echoRest.Group("/resources")
	groupResources.GET("/", ListResources)
	groupResources.GET("/:id", GetResource)
	groupResources.POST("/:id", UpdateResource)
	groupResources.PUT("/", UploadResource)
	groupResources.DELETE("/:id", DeleteResource)

	var serverConf = config.CurrentConfig.Server
	echoRest.Logger.Fatal(echoRest.Start(fmt.Sprintf("%s:%d", serverConf.Host, serverConf.Port)))
}

func GetDocumentDescriptions(c echo.Context) error {
	return c.JSONPretty(http.StatusOK, backend.DocumentTree, " ")
}

func GetDocumentDescription(c echo.Context) error {
	id := c.Param(paramId)

	d := backend.GetDocument(id)

	if d != nil {
		return c.JSONPretty(http.StatusOK, d, "  ")
	} else {
		return c.NoContent(http.StatusNotFound)
	}
}

func GetDocumentContent(c echo.Context) error {
	id := c.Param(paramId)

	d := backend.GetDocument(id)

	if d != nil {
		return c.File(d.Path)
	} else {
		return c.NoContent(http.StatusNotFound)
	}
}

func createError(c echo.Context) error {
	e := &Error{
		Message: "Something went terribly wrong! :(",
	}

	return c.JSONPretty(http.StatusInternalServerError, e, "  ")
}

func UpdateDocument(c echo.Context) error {
	id := c.Param(paramId)
	return c.String(http.StatusOK, "Document ID: "+id)
}

func CreateDocument(c echo.Context) error {
	newDocumentRequest := new(NewDocumentRequest)
	if err := c.Bind(newDocumentRequest); err != nil {
		return err
	}

	return c.String(http.StatusOK, "Document Created")
}

func DeleteDocument(c echo.Context) error {
	id := c.Param(paramId)

	backend.DeleteDocument(id)

	return c.String(http.StatusOK, "Document ID: "+id)
}

func ListResources(c echo.Context) error {
	id := c.Param(paramId)

	return c.JSON(http.StatusOK, "Resource ID: "+id)
}
func GetResource(c echo.Context) error {
	id := c.Param(paramId)
	return c.String(http.StatusOK, "Resource ID: "+id)
}
func UpdateResource(c echo.Context) error {
	id := c.Param(paramId)
	return c.String(http.StatusOK, "Resource ID: "+id)
}
func UploadResource(c echo.Context) error {
	id := c.Param(paramId)
	return c.String(http.StatusOK, "Resource ID: "+id)
}
func DeleteResource(c echo.Context) error {
	id := c.Param(paramId)
	return c.String(http.StatusOK, "Resource ID: "+id)
}
