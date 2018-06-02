package backend

import (
	"mkdocsrest/config"
	"path/filepath"
	"os"
	"github.com/OneOfOne/xxhash"
	"strconv"
	"io/ioutil"
	"log"
	"strings"
	"time"
)

const (
	TypeSection  = "section"
	TypeDocument = "document"
	TypeResource = "resource"
)

type (
	Section struct {
		Type        string      `json:"type" xml:"type" form:"type" query:"type"`
		ID          string      `json:"id" xml:"id" form:"id" query:"id"`
		Name        string      `json:"name" xml:"name" form:"name" query:"name"`
		Path        string      `json:"path" xml:"path" form:"path" query:"path"`
		Subsections *[]Section  `json:"subsections" xml:"subsections" form:"subsections" query:"subsections"`
		Documents   *[]Document `json:"documents" xml:"documents" form:"documents" query:"documents"`
		Resources   *[]Resource `json:"resources" xml:"resources" form:"resources" query:"resources"`
	}

	Document struct {
		Type     string    `json:"type" xml:"type" form:"type" query:"type"`
		ID       string    `json:"id" xml:"id" form:"id" query:"id"`
		Name     string    `json:"name" xml:"name" form:"name" query:"name"`
		Path     string    `json:"path" xml:"path" form:"path" query:"path"`
		Filesize int64     `json:"filesize" xml:"filesize" form:"filesize" query:"filesize"`
		ModTime  time.Time `json:"modtime" xml:"modtime" form:"modtime" query:"modtime"`
	}

	Resource struct {
		Type     string    `json:"type" xml:"type" form:"type" query:"type"`
		ID       string    `json:"id" xml:"id" form:"id" query:"id"`
		Name     string    `json:"name" xml:"name" form:"name" query:"name"`
		Path     string    `json:"path" xml:"path" form:"path" query:"path"`
		Filesize int64     `json:"filesize" xml:"filesize" form:"filesize" query:"filesize"`
		ModTime  time.Time `json:"modtime" xml:"modtime" form:"modtime" query:"modtime"`
	}
)

// an in memory representation of the mkdocs file structure
var DocumentTree Section

// traverses the mkdocs directory and puts all files into the cache
func CreateDocumentTree() {
	DocumentTree = createSectionForTree("", "root")
	searchDir := config.CurrentConfig.MkDocs.DocsPath
	populateDocumentTree(&DocumentTree, searchDir)
}

func populateDocumentTree(section *Section, path string) {
	files, err := ioutil.ReadDir(path)
	if err != nil {
		log.Fatal(err)
	}

	for _, f := range files {
		if f.IsDir() {
			var subSection = createSectionForTree(path, f.Name())
			*section.Subsections = append(*section.Subsections, subSection)

			// recursive call
			populateDocumentTree(&subSection, filepath.Join(path, subSection.Name))
		} else {
			if strings.HasSuffix(f.Name(), ".md") {
				var document = createDocumentForTree(path, f)
				*section.Documents = append(*section.Documents, document)
			} else {
				var resource = createResourceForTree(path, f)
				*section.Resources = append(*section.Resources, resource)
			}
		}
	}
}

// creates a section object for storing in the tree
func createSectionForTree(path string, name string) Section {
	return Section{
		Type:        TypeSection,
		ID:          createHash(path),
		Name:        name,
		Path:        path,
		Documents:   &[]Document{},
		Subsections: &[]Section{},
		Resources:   &[]Resource{},
	}
}

// creates a (non-cryptographic) hash of the given string
func createHash(s string) string {
	var pathHash = xxhash.ChecksumString64(s)
	return strconv.FormatUint(pathHash, 10)
}

// creates a document object for storing in the tree
func createDocumentForTree(path string, f os.FileInfo) Document {
	var fileName = f.Name()
	var fileSize = f.Size()
	var fileModTime = f.ModTime()
	var documentPath = filepath.Join(path, f.Name())

	return Document{
		Type:     TypeDocument,
		ID:       createHash(documentPath),
		Name:     fileName,
		Path:     documentPath,
		Filesize: fileSize,
		ModTime:  fileModTime,
	}
}

// creates a resource object for storing in the tree
func createResourceForTree(path string, f os.FileInfo) Resource {
	var fileName = f.Name()
	var fileSize = f.Size()
	var fileModTime = f.ModTime()
	var resourcePath = filepath.Join(path, f.Name())

	return Resource{
		Type:     TypeResource,
		ID:       createHash(resourcePath),
		Name:     fileName,
		Path:     resourcePath,
		Filesize: fileSize,
		ModTime:  fileModTime,
	}
}

// finds a document with the given id in the document tree
func GetSection(id string) *Section {
	return findSectionRecursive(&DocumentTree, id)
}

// finds a document with the given id in the document tree
func GetDocument(id string) *Document {
	return findDocumentRecursive(&DocumentTree, id)
}

// finds a resource with the given id in the document tree
func GetResource(id string) *Resource {
	return findResourceRecursive(&DocumentTree, id)
}

// traverses the tree and searches for a document with the given id
func findSectionRecursive(section *Section, id string) *Section {
	if section.ID == id {
		return section
	}

	for _, subsection := range *section.Subsections {
		d := findSectionRecursive(&subsection, id)
		if d != nil {
			return d
		}
	}

	return nil
}

// traverses the tree and searches for a document with the given id
func findDocumentRecursive(section *Section, id string) *Document {
	for _, document := range *section.Documents {
		if document.ID == id {
			return &document
		}
	}

	for _, subsection := range *section.Subsections {
		d := findDocumentRecursive(&subsection, id)
		if d != nil {
			return d
		}
	}

	return nil
}

// traverses the tree and searches for a resource with the given id
func findResourceRecursive(section *Section, id string) *Resource {
	for _, resource := range *section.Resources {
		if resource.ID == id {
			return &resource
		}
	}

	for _, subsection := range *section.Subsections {
		r := findResourceRecursive(&subsection, id)
		if r != nil {
			return r
		}
	}

	return nil
}

// creates a new section as a child of the given parent section id and the given name
func CreateSection(parentSectionId string, sectionName string) (err error) {
	return nil
}

// creates a new document as a child of the given parent section id and the given name
func CreateDocument(parentSectionId string, documentName string) (err error) {
	return nil
}

// deletes a file/folder with the given ID and type
func DeleteItem(id string, fileType string) (success bool, err error) {
	d := findDocumentRecursive(&DocumentTree, id)
	if d != nil && d.Type == fileType {
		return DeleteFile(d.Path)
	} else {
		return false, nil
	}
}
