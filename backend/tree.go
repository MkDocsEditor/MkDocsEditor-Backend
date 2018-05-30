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
)

const sectionType = "section"
const documentType = "document"
const resourceType = "resource"

type Section struct {
	Type        string      `json:"type" xml:"type" form:"type" query:"type"`
	Name        string      `json:"name" xml:"name" form:"name" query:"name"`
	Subsections *[]Section  `json:"subsections" xml:"subsections" form:"subsections" query:"subsections"`
	Documents   *[]Document `json:"documents" xml:"documents" form:"documents" query:"documents"`
}

type Document struct {
	Type     string `json:"type" xml:"type" form:"type" query:"type"`
	ID       string `json:"id" xml:"id" form:"id" query:"id"`
	Name     string `json:"name" xml:"name" form:"name" query:"name"`
	Path     string `json:"path" xml:"path" form:"path" query:"path"`
	Filesize int64  `json:"filesize" xml:"filesize" form:"filesize" query:"filesize"`
	Content  string `json:"content" xml:"content" form:"content" query:"content"`
}

// an in memory representation of the mkdocs file structure
var DocumentTree = &Section{
	Type:        sectionType,
	Name:        "root",
	Documents:   &[]Document{},
	Subsections: &[]Section{},
}

// traverses the mkdocs directory and puts all files into the cache
func CreateDocumentTree() {
	searchDir := config.CurrentConfig.MkDocs.Path
	populateDocumentTree(DocumentTree, filepath.Join(searchDir, "docs"))
}

func populateDocumentTree(section *Section, path string) {
	files, err := ioutil.ReadDir(path)
	if err != nil {
		log.Fatal(err)
	}

	for _, f := range files {
		if f.IsDir() {
			var subSection = createSectionForTree(path, f)
			*section.Subsections = append(*section.Subsections, subSection)

			// recursive call
			populateDocumentTree(&subSection, filepath.Join(path, subSection.Name))
		} else {
			if strings.HasSuffix(f.Name(), ".md") {
				var document = createDocumentForTree(path, f)
				*section.Documents = append(*section.Documents, document)
			}
		}
	}
}

func createSectionForTree(path string, info os.FileInfo) Section {
	return Section{
		Type:        sectionType,
		Name:        info.Name(),
		Documents:   &[]Document{},
		Subsections: &[]Section{},
	}
}

func createDocumentForTree(path string, f os.FileInfo) Document {
	var documentPath = filepath.Join(path, f.Name())

	var pathHash = xxhash.ChecksumString64(documentPath)
	var pathHashAsString = strconv.FormatUint(pathHash, 10)

	var fileName = f.Name()
	var fileSize = f.Size()

	return Document{
		Type:     documentType,
		ID:       pathHashAsString,
		Name:     fileName,
		Path:     documentPath,
		Filesize: fileSize,
		Content:  "", // only filled on single "get" requests
	}
}

func GetDocument(id string) *Document {
	d := findRecursive(DocumentTree, id)
	if d != nil {
		var content, err = ReadFile(d.Path)
		if err != nil {
			panic(err)
		}

		d.Content = content
	}

	return d
}

// traverses the document tree and searches for a document with the given id
func findRecursive(section *Section, id string) *Document {
	for _, document := range *section.Documents {
		if document.ID == id {
			return &document
		}
	}

	for _, subsection := range *section.Subsections {
		return findRecursive(&subsection, id)
	}

	return nil
}

// deletes a document
func DeleteDocument(id string) {
	d := findRecursive(DocumentTree, id)
	if d != nil && d.Type == documentType {
		DeleteFile(d.Path)
	}
}
