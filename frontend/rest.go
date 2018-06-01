package frontend

import (
	"github.com/labstack/echo"
	"net/http"
	"mkdocsrest/backend"
	"os"
	"io"
	"mkdocsrest/config"
	"github.com/labstack/echo/middleware"
	"fmt"
)

const urlParamId = "id"

func SetupRestService() {
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

	echoRest.GET("/tree", getTree)
	echoRest.GET("/tree/", getTree)

	// Authentication
	// Group level middleware
	groupDocuments := echoRest.Group("/document")

	groupDocuments.GET("/:id", getDocumentDescription)
	groupDocuments.GET("/:id/name", getDocumentName)
	groupDocuments.GET("/:id/content", getDocumentContent)

	groupDocuments.POST("/:id/name", updateDocumentName)
	groupDocuments.POST("/:id/content", updateDocumentContent)
	groupDocuments.PUT("/", createDocument)
	groupDocuments.DELETE("/:id", deleteDocument)

	groupResources := echoRest.Group("/resource")
	groupResources.GET("", listResources)

	groupResources.GET("/:id", getResourceContent)
	groupResources.POST("/:id", updateResource)
	groupResources.PUT("/", uploadResource)
	groupResources.DELETE("/:id", deleteResource)

	var serverConf = config.CurrentConfig.Server
	echoRest.Logger.Fatal(echoRest.Start(fmt.Sprintf("%s:%d", serverConf.Host, serverConf.Port)))
}

// returns the complete file tree
func getTree(c echo.Context) error {
	return c.JSONPretty(http.StatusOK, backend.DocumentTree, " ")
}

// returns the description of a single document (if found)
func getDocumentDescription(c echo.Context) error {
	id := c.Param(urlParamId)

	d := backend.GetDocument(id)

	if d != nil {
		return c.JSONPretty(http.StatusOK, d, "  ")
	} else {
		return c.NoContent(http.StatusNotFound)
	}
}

func getDocumentName(c echo.Context) error {
	id := c.Param(urlParamId)

	d := backend.GetDocument(id)

	if d != nil {
		return c.HTML(http.StatusOK, d.Name)
	} else {
		return c.NoContent(http.StatusNotFound)
	}
}

// returns the content of the document with the given id (if found)
func getDocumentContent(c echo.Context) error {
	id := c.Param(urlParamId)

	d := backend.GetDocument(id)

	if d != nil {
		return c.File(d.Path)
	} else {
		return c.NoContent(http.StatusNotFound)
	}
}

// updates the name of a document
func updateDocumentName(c echo.Context) error {
	id := c.Param(urlParamId)
	return c.String(http.StatusOK, "Document ID: "+id)
}

// updates the content of the document with the given id (if found)
// if the document doesn't exist
func updateDocumentContent(c echo.Context) error {
	id := c.Param(urlParamId)

	d := backend.GetDocument(id)
	if d == nil {
		return c.NoContent(http.StatusNotFound)
	}

	// Source
	file, err := c.FormFile("file")
	if err != nil {
		return err
	}
	src, err := file.Open()
	if err != nil {
		return err
	}
	defer src.Close()

	// Destination
	dst, err := os.Create(d.Path)
	if err != nil {
		return err
	}
	defer dst.Close()

	// Copy
	if _, err = io.Copy(dst, src); err != nil {
		return err
	}

	return c.NoContent(http.StatusOK)
}

// creates a new document with the given data
func createDocument(c echo.Context) error {
	// todo: specify path and name somehow

	backend.CreateDocument("", "")

	return c.String(http.StatusOK, "Document Created")
}

// deletes an existing document
func deleteDocument(c echo.Context) error {
	id := c.Param(urlParamId)

	backend.DeleteDocument(id)

	return c.String(http.StatusOK, "Document ID: "+id)
}

// returns a list of all resources in the tree
func listResources(c echo.Context) error {
	id := c.Param(urlParamId)

	return c.JSON(http.StatusOK, "Resource ID: "+id)
}

// returns the description of a single resource with the given id (if found)
func GetResourceDescription(c echo.Context) error {
	id := c.Param(urlParamId)
	return c.String(http.StatusOK, "Resource ID: "+id)
}

// returns the content of a single resource with the given id (if found)
func getResourceContent(c echo.Context) error {
	id := c.Param(urlParamId)
	return c.String(http.StatusOK, "Resource ID: "+id)
}

// updates an existing resource file
func updateResource(c echo.Context) error {
	id := c.Param(urlParamId)
	return c.String(http.StatusOK, "Resource ID: "+id)
}

// uploads a new resource file
func uploadResource(c echo.Context) error {
	id := c.Param(urlParamId)
	return c.String(http.StatusOK, "Resource ID: "+id)
}

// deletes a resource file
func deleteResource(c echo.Context) error {
	id := c.Param(urlParamId)
	return c.String(http.StatusOK, "Resource ID: "+id)
}
