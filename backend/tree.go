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
	"mkdocsrest/backend/diff"
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
		Path        string      `json:"-" xml:"-" form:"-" query:"-"`
		Subsections *[]Section  `json:"subsections" xml:"subsections" form:"subsections" query:"subsections"`
		Documents   *[]Document `json:"documents" xml:"documents" form:"documents" query:"documents"`
		Resources   *[]Resource `json:"resources" xml:"resources" form:"resources" query:"resources"`
	}

	Document struct {
		Type     string    `json:"type" xml:"type" form:"type" query:"type"`
		ID       string    `json:"id" xml:"id" form:"id" query:"id"`
		Name     string    `json:"name" xml:"name" form:"name" query:"name"`
		Path     string    `json:"-" xml:"-" form:"-" query:"-"`
		Filesize int64     `json:"filesize" xml:"filesize" form:"filesize" query:"filesize"`
		ModTime  time.Time `json:"modtime" xml:"modtime" form:"modtime" query:"modtime"`
		Content  string    `json:"-" xml:"-" form:"-" query:"-"`
	}

	Resource struct {
		Type     string    `json:"type" xml:"type" form:"type" query:"type"`
		ID       string    `json:"id" xml:"id" form:"id" query:"id"`
		Name     string    `json:"name" xml:"name" form:"name" query:"name"`
		Path     string    `json:"-" xml:"-" form:"-" query:"-"`
		Filesize int64     `json:"filesize" xml:"filesize" form:"filesize" query:"filesize"`
		ModTime  time.Time `json:"modtime" xml:"modtime" form:"modtime" query:"modtime"`
	}
)

// an in memory representation of the mkdocs file structure
var DocumentTree Section

// traverses the mkdocs directory and puts all files into a tree representation
func CreateItemTree() {
	_, file := filepath.Split(config.CurrentConfig.MkDocs.DocsPath)

	DocumentTree = createSectionForTree(config.CurrentConfig.MkDocs.DocsPath, file)
	searchDir := config.CurrentConfig.MkDocs.DocsPath
	populateItemTree(&DocumentTree, searchDir)
}

// recursive function that creates a subtree of the complete item tree
func populateItemTree(section *Section, path string) {
	files, err := ioutil.ReadDir(path)
	if err != nil {
		log.Fatal(err)
	}

	for _, f := range files {
		if f.IsDir() {
			var subSection = createSectionForTree(path, f.Name())
			*section.Subsections = append(*section.Subsections, subSection)

			// recursive call
			populateItemTree(&subSection, filepath.Join(path, subSection.Name))
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
	var sectionPath = filepath.Join(path, name)

	return Section{
		Type:        TypeSection,
		ID:          createHash(sectionPath),
		Name:        name,
		Path:        sectionPath,
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
func createDocumentForTree(path string, f os.FileInfo) (document Document) {
	var fileName = f.Name()
	var fileSize = f.Size()
	var fileModTime = f.ModTime()
	var documentPath = filepath.Join(path, f.Name())

	content, err := ReadFile(documentPath)
	if err != nil {
		log.Fatal(err)
	}

	return Document{
		Type:     TypeDocument,
		ID:       createHash(documentPath),
		Name:     fileName,
		Path:     documentPath,
		Filesize: fileSize,
		ModTime:  fileModTime,
		Content:  content,
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
func CreateSection(parentPath string, sectionName string) (err error) {
	return os.MkdirAll(filepath.Join(parentPath, sectionName), os.ModeDir)
}

// creates a new document as a child of the given parent section id and the given name
func CreateDocument(parentSectionId string, documentName string) (err error) {
	return nil
}

// Applies the given patch to the document with the given id
func ApplyPatch(document *Document, patchesAsText string) (result string, err error) {
	result, err = diff.ApplyPatch(document.Content, patchesAsText)

	if err != nil {
		log.Fatal(err)
	}

	document.Content = result

	return result, err
}

// deletes a file/folder with the given ID and type from disk
func DeleteItem(id string, itemType string) (success bool, err error) {
	var path string
	switch itemType {
	case TypeSection:
		s := findSectionRecursive(&DocumentTree, id)
		if s != nil {
			path = s.Path
		} else {
			return false, nil
		}
	case TypeDocument:
		d := findDocumentRecursive(&DocumentTree, id)
		if d != nil {
			path = d.Path
		} else {
			return false, nil
		}
	case TypeResource:
		r := findResourceRecursive(&DocumentTree, id)
		if r != nil {
			path = r.Path
		} else {
			return false, nil
		}
	}

	return DeleteFileOrFolder(path)
}
