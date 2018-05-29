package main

import (
	"github.com/labstack/echo"
	"net/http"
	"mkdocsrest/config"
	"github.com/labstack/echo/middleware"
	"fmt"
)

type Document struct {
	ID      string `json:"id" xml:"id" form:"id" query:"id"`
	Name    string `json:"name" xml:"name" form:"name" query:"name"`
	Content string `json:"content" xml:"content" form:"content" query:"content"`
}

// our main function
func main() {
	config.Setup()

	setupRestService()
}

func setupRestService() {
	echoRest := echo.New()

	// Root level middleware
	echoRest.Use(middleware.Logger())
	echoRest.Use(middleware.Recover())

	// global auth needed
	echoRest.Use(middleware.BasicAuth(func(username, password string, c echo.Context) (bool, error) {
		if username == "joe" && password == "secret" {
			return true, nil
		}
		return false, nil
	}))

	// Authentication
	// Group level middleware
	groupDocuments := echoRest.Group("/documents")
	groupDocuments.GET("/", GetDocuments)
	groupDocuments.GET("/:id", GetDocument)
	groupDocuments.POST("/:id", UpdateDocument)
	groupDocuments.PUT("/", CreateDocument)
	groupDocuments.DELETE("/:id", DeleteDocument)

	groupResources := echoRest.Group("/resources")
	groupResources.GET("/", ListResources)
	groupResources.GET("/:id", GetResource)
	groupResources.POST("/:id", UpdateResource)
	groupResources.PUT("/", UploadResource)
	groupResources.DELETE("/:id", DeleteResource)

	echoRest.Logger.Fatal(echoRest.Start(fmt.Sprintf(":%d", config.CurrentConfig.Server.Port)))
}

func GetDocuments(c echo.Context) error {
	return c.String(http.StatusOK, "Hello, World!")
}

func GetDocument(c echo.Context) error {
	id := c.Param("id")

	d := &Document{
		ID:      id,
		Name:    "Test.md",
		Content: "# Header",
	}

	return c.JSONPretty(http.StatusOK, d, "  ")
}

func UpdateDocument(c echo.Context) error {
	id := c.Param("id")
	return c.String(http.StatusOK, "Document ID: "+id)
}

func CreateDocument(c echo.Context) error {
	id := c.Param("id")

	document := new(Document)
	if err := c.Bind(document); err != nil {
		return err
	}

	return c.String(http.StatusOK, "Document ID: "+id)
}

func DeleteDocument(c echo.Context) error {
	id := c.Param("id")
	return c.String(http.StatusOK, "Document ID: "+id)
}

func ListResources(c echo.Context) error {
	id := c.Param("id")

	return c.JSON(http.StatusOK, "Resource ID: "+id)
}
func GetResource(c echo.Context) error {
	id := c.Param("id")
	return c.String(http.StatusOK, "Resource ID: "+id)
}
func UpdateResource(c echo.Context) error {
	id := c.Param("id")
	return c.String(http.StatusOK, "Resource ID: "+id)
}
func UploadResource(c echo.Context) error {
	id := c.Param("id")
	return c.String(http.StatusOK, "Resource ID: "+id)
}
func DeleteResource(c echo.Context) error {
	id := c.Param("id")
	return c.String(http.StatusOK, "Resource ID: "+id)
}
