package backend

import (
	"errors"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/config"
	"github.com/OneOfOne/xxhash"
	"io/ioutil"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	TypeSection  = "section"
	TypeDocument = "document"
	TypeResource = "resource"

	markdownFileExtension = ".md"
)

type (
	Section struct {
		Type        string       `json:"type" xml:"type" form:"type" query:"type"`
		ID          string       `json:"id" xml:"id" form:"id" query:"id"`
		Name        string       `json:"name" xml:"name" form:"name" query:"name"`
		Path        string       `json:"-" xml:"-" form:"-" query:"-"`
		Subsections *[]*Section  `json:"subsections" xml:"subsections" form:"subsections" query:"subsections"`
		Documents   *[]*Document `json:"documents" xml:"documents" form:"documents" query:"documents"`
		Resources   *[]*Resource `json:"resources" xml:"resources" form:"resources" query:"resources"`
	}

	Document struct {
		Type     string    `json:"type" xml:"type" form:"type" query:"type"`
		ID       string    `json:"id" xml:"id" form:"id" query:"id"`
		Name     string    `json:"name" xml:"name" form:"name" query:"name"`
		Path     string    `json:"-" xml:"-" form:"-" query:"-"`
		Filesize int64     `json:"filesize" xml:"filesize" form:"filesize" query:"filesize"`
		ModTime  time.Time `json:"modtime" xml:"modtime" form:"modtime" query:"modtime"`
		Content  string    `json:"-" xml:"-" form:"-" query:"-"`
		SubUrl   string    `json:"url" xml:"url" form:"url" query:"url"`
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
var rootPath string

func init() {
	rootPath = config.CurrentConfig.MkDocs.DocsPath
	CreateItemTree()
}

// traverses the mkdocs directory and puts all files into a tree representation
func CreateItemTree() {
	path, file := filepath.Split(rootPath)

	DocumentTree = createSectionForTree(path, file, "root")
	searchDir := DocumentTree.Path
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
			var subSection = createSectionForTree(path, f.Name(), "")
			*section.Subsections = append(*section.Subsections, &subSection)

			// recursive call
			populateItemTree(&subSection, filepath.Join(path, subSection.Name))
		} else {
			if strings.HasSuffix(f.Name(), markdownFileExtension) {
				var document = createDocumentForTree(path, f)
				*section.Documents = append(*section.Documents, &document)
			} else {
				var resource = createResourceForTree(path, f)
				*section.Resources = append(*section.Resources, &resource)
			}
		}
	}
}

// creates a (non-cryptographic) hash of the given string
func createHash(s string) string {
	var pathHash = xxhash.ChecksumString64(s)
	return strconv.FormatUint(pathHash, 10)
}

// creates a section object for storing in the tree
func createSectionForTree(path string, name string, id string) Section {
	var sectionPath = filepath.Join(path, name)

	if id == "" {
		id = generateId(sectionPath)
	}

	return Section{
		Type:        TypeSection,
		ID:          id,
		Name:        name,
		Path:        sectionPath,
		Documents:   &[]*Document{},
		Subsections: &[]*Section{},
		Resources:   &[]*Resource{},
	}
}

// generates an item id from its path
func generateId(path string) string {
	return createHash(path)
}

// creates a document object for storing in the tree
func createDocumentForTree(parentFolderPath string, f os.FileInfo) (document Document) {
	var fileName = f.Name()
	var fileSize = f.Size()
	var fileModTime = f.ModTime()
	var documentPath = filepath.Join(parentFolderPath, f.Name())

	var subUrl = ""
	for _, element := range strings.Split(strings.TrimPrefix(documentPath, rootPath), string(filepath.Separator)) {
		element = strings.TrimSuffix(element, markdownFileExtension)
		if len(element) > 0 {
			subUrl += url.PathEscape(element) + "/"
		}
	}

	content, err := ReadFile(documentPath)
	if err != nil {
		log.Fatal(err)
	}

	return Document{
		Type:     TypeDocument,
		ID:       generateId(documentPath),
		Name:     fileName[0 : len(fileName)-len(markdownFileExtension)],
		Path:     documentPath,
		Filesize: fileSize,
		ModTime:  fileModTime,
		Content:  content,
		SubUrl:   subUrl,
	}
}

// creates a resource object for storing in the tree
func createResourceForTree(parentFolderPath string, f os.FileInfo) Resource {
	var fileName = f.Name()
	var fileSize = f.Size()
	var fileModTime = f.ModTime()
	var resourcePath = filepath.Join(parentFolderPath, f.Name())

	return Resource{
		Type:     TypeResource,
		ID:       generateId(resourcePath),
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
		d := findSectionRecursive(subsection, id)
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
			return document
		}
	}

	for _, subsection := range *section.Subsections {
		d := findDocumentRecursive(subsection, id)
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
			return resource
		}
	}

	for _, subsection := range *section.Subsections {
		r := findResourceRecursive(subsection, id)
		if r != nil {
			return r
		}
	}

	return nil
}

// creates a new section as a child of the given parent section id and the given name
func CreateSection(parentPath string, sectionName string) (section *Section, err error) {
	sectionId := generateId(parentPath)
	parentSection := findSectionRecursive(&DocumentTree, sectionId)
	if parentSection == nil {
		log.Fatalf("Parent section %s not found", parentPath)
	}

	newSection := createSectionForTree(parentPath, sectionName, "")

	// create folder
	err = os.MkdirAll(newSection.Path, os.ModeDir)
	if err != nil {
		return nil, err
	}

	// append section to tree
	*parentSection.Subsections = append(*parentSection.Subsections, &newSection)

	return &newSection, err
}

// creates a new document as a child of the given parent section id and the given name
func CreateDocument(parentSectionId string, documentName string) (document *Document, err error) {
	parent := findSectionRecursive(&DocumentTree, parentSectionId)

	if parent == nil {
		return nil, errors.New("Parent section " + parentSectionId + " does not exist")
	}

	var fileName = documentName + markdownFileExtension

	filePath := filepath.Join(parent.Path, fileName)

	exists, err := fileExists(filePath)
	if exists {
		return nil, errors.New("Target document " + documentName + " already exists!")
	}

	f, err := os.Create(filePath)
	if err != nil {
		return nil, err
	}
	fileInfo, err := f.Stat()
	if err != nil {
		return nil, err
	}

	newDocumentTreeItem := createDocumentForTree(parent.Path, fileInfo)
	*parent.Documents = append(*parent.Documents, &newDocumentTreeItem)

	return &newDocumentTreeItem, err
}

func fileExists(filePath string) (exists bool, err error) {
	if _, err := os.Stat(filePath); err == nil {
		// path/to/whatever exists
		return true, nil
	} else if os.IsNotExist(err) {
		return false, nil
	} else {
		return exists, err
	}
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

	// TODO: check if anyone is editing files in here before actually deleting it

	// TODO: remove the deleted item from document tree
	return DeleteFileOrFolder(path)
}
