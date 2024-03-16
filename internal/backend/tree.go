package backend

import (
	"errors"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
	"github.com/OneOfOne/xxhash"
	"io"
	"log"
	"mime/multipart"
	"net/url"
	"os"
	"path/filepath"
	"slices"
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

// DocumentTree an in memory representation of the mkdocs file structure
var DocumentTree Section
var rootPath string

func InitWikiTree() {
	rootPath = configuration.CurrentConfig.MkDocs.DocsPath
	CreateItemTree()
}

// CreateItemTree traverses the mkdocs directory and puts all files into a tree representation
func CreateItemTree() {
	path, file := filepath.Split(rootPath)

	DocumentTree = createSectionForTree(path, file, "root")
	searchDir := DocumentTree.Path
	populateItemTree(&DocumentTree, searchDir)
}

// recursive function that creates a subtree of the complete item tree
func populateItemTree(section *Section, path string) {
	files, err := os.ReadDir(path)
	if err != nil {
		log.Fatal(err)
	}

	for _, f := range files {
		totalPath := filepath.Join(path, f.Name())
		if isOnIgnoreList(totalPath) {
			continue
		}

		if f.IsDir() {
			var subSection = createSectionForTree(path, f.Name(), "")
			*section.Subsections = append(*section.Subsections, &subSection)

			// recursive call
			populateItemTree(&subSection, filepath.Join(path, subSection.Name))
		} else {
			f, _ := f.Info()
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

func isOnIgnoreList(path string) bool {
	mkDocsConfig, err := readMkDocsConfig()
	if err != nil {
		return false
	}

	pathInRootPath := strings.TrimPrefix(path, rootPath)
	pathInRootPath = strings.TrimPrefix(pathInRootPath, "/")

	shouldIgnore := slices.Contains(mkDocsConfig.ExtraCss, pathInRootPath)
	shouldIgnore = shouldIgnore || slices.Contains(configuration.CurrentConfig.MkDocs.Blacklist, pathInRootPath)

	return shouldIgnore
}

// creates a (non-cryptographic) hash of the given string
func createHash(s string) string {
	pathHash := xxhash.ChecksumString64(s)
	return strconv.FormatUint(pathHash, 10)
}

// creates a section object for storing in the tree
func createSectionForTree(path string, name string, id string) Section {
	sectionPath := filepath.Join(path, name)

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
	fileName := f.Name()
	fileSize := f.Size()
	fileModTime := f.ModTime()
	documentPath := filepath.Join(parentFolderPath, f.Name())

	subUrl := ""
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
	fileName := f.Name()
	fileSize := f.Size()
	fileModTime := f.ModTime()
	resourcePath := filepath.Join(parentFolderPath, f.Name())

	return Resource{
		Type:     TypeResource,
		ID:       generateId(resourcePath),
		Name:     fileName,
		Path:     resourcePath,
		Filesize: fileSize,
		ModTime:  fileModTime,
	}
}

// GetSection finds a document with the given id in the document tree
func GetSection(id string) *Section {
	return findSectionRecursive(&DocumentTree, id)
}

// GetDocument finds a document with the given id in the document tree
func GetDocument(id string) *Document {
	return findDocumentRecursive(&DocumentTree, id)
}

// GetResource finds a resource with the given id in the document tree
func GetResource(id string) *Resource {
	return findResourceRecursive(&DocumentTree, id)
}

func CreateResource(parentSectionId string, resourceName string, content string) (resource *Resource, err error) {
	parent := findSectionRecursive(&DocumentTree, parentSectionId)

	if parent == nil {
		return nil, errors.New("Parent section " + parentSectionId + " does not exist")
	}

	var fileName = resourceName
	filePath := filepath.Join(parent.Path, fileName)

	exists, err := fileExists(filePath)
	if exists {
		return nil, errors.New("Target resource " + resourceName + " already exists!")
	}

	dst, err := os.Create(filePath)
	if err != nil {
		return nil, err
	}
	defer dst.Close()

	if _, err = io.WriteString(dst, content); err != nil {
		return nil, err
	}

	fileInfo, err := dst.Stat()
	if err != nil {
		return nil, err
	}

	newResourceTreeItem := createResourceForTree(parent.Path, fileInfo)
	*parent.Resources = append(*parent.Resources, &newResourceTreeItem)

	return &newResourceTreeItem, err
}

func CreateResourceFromMultipart(parentSectionId string, resourceName string, src multipart.File) (resource *Resource, err error) {
	defer src.Close()

	parent := findSectionRecursive(&DocumentTree, parentSectionId)

	if parent == nil {
		return nil, errors.New("Parent section " + parentSectionId + " does not exist")
	}

	var fileName = resourceName

	filePath := filepath.Join(parent.Path, fileName)

	exists, err := fileExists(filePath)
	if exists {
		return nil, errors.New("Target resource " + resourceName + " already exists!")
	}

	dst, err := os.Create(filePath)
	if err != nil {
		return nil, err
	}
	defer dst.Close()

	if _, err = io.Copy(dst, src); err != nil {
		return nil, err
	}

	fileInfo, err := dst.Stat()
	if err != nil {
		return nil, err
	}

	newResourceTreeItem := createResourceForTree(parent.Path, fileInfo)
	*parent.Resources = append(*parent.Resources, &newResourceTreeItem)

	return &newResourceTreeItem, err
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

// CreateSection creates a new section with the given name as a child of the given parent section
func CreateSection(parentSection *Section, sectionName string) (section *Section, err error) {
	newSection := createSectionForTree(parentSection.Path, sectionName, "")

	// create folder
	err = os.MkdirAll(newSection.Path, os.ModePerm)
	if err != nil {
		return nil, err
	}

	// append section to tree
	*parentSection.Subsections = append(*parentSection.Subsections, &newSection)

	return &newSection, err
}

// CreateDocument creates a new document as a child of the given parent section id and the given name
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

func RenameSection(section *Section, name string) (sec *Section, err error) {
	var newFilePath = filepath.Join(filepath.Dir(section.Path), name)
	exists, err := fileExists(newFilePath)
	if exists {
		return nil, errors.New("Target section " + section.Name + " already exists!")
	}

	err = os.Rename(section.Path, newFilePath)
	if err != nil {
		return nil, err
	}

	CreateItemTree()
	section = GetSection(generateId(newFilePath))

	return section, err
}

func RenameDocument(document *Document, name string) (doc *Document, err error) {
	var fileName = name + markdownFileExtension
	var newFilePath = filepath.Join(filepath.Dir(document.Path), fileName)

	exists, err := fileExists(newFilePath)
	if exists {
		return nil, errors.New("Target document " + document.Name + " already exists!")
	}

	err = os.Rename(document.Path, newFilePath)
	if err != nil {
		return nil, err
	}

	CreateItemTree()
	document = GetDocument(generateId(newFilePath))

	return document, err
}

func RenameResource(resource *Resource, name string) (res *Resource, err error) {
	var newFilePath = filepath.Join(filepath.Dir(resource.Path), name)
	exists, err := fileExists(newFilePath)
	if exists {
		return nil, errors.New("Target resource " + resource.Name + " already exists!")
	}

	err = os.Rename(resource.Path, newFilePath)
	if err != nil {
		return nil, err
	}

	CreateItemTree()
	resource = GetResource(generateId(newFilePath))

	return resource, err
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

// DeleteItem deletes a file/folder with the given ID and type from disk
func DeleteItem(id string, itemType string) (success bool, err error) {
	var path string
	switch itemType {
	case TypeSection:
		s := findSectionRecursive(&DocumentTree, id)
		if s != nil {
			path = s.Path

			// check if any documents in this section are currently being edited
			err = IsItemBeingEditedRecursive(s)
			if err != nil {
				return false, err
			}
		} else {
			return false, nil
		}
	case TypeDocument:
		d := findDocumentRecursive(&DocumentTree, id)
		if d != nil {
			path = d.Path
			if IsClientConnected(id) {
				return false, errors.New("document is currently being edited by another user")
			}
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

	success, err = DeleteFileOrFolder(path)
	if !success || err != nil {
		return success, err
	}

	err = removeNodeFromTree(&DocumentTree, id)

	return success, err
}

func IsItemBeingEditedRecursive(s *Section) (err error) {
	for _, doc := range *s.Documents {
		if IsClientConnected(doc.ID) {
			return errors.New("a document within this section is currently being edited by another user")
		}
	}

	for _, subsection := range *s.Subsections {
		err = IsItemBeingEditedRecursive(subsection)
		if err != nil {
			return err
		}
	}

	return nil
}

func removeNodeFromTree(s *Section, id string) (err error) {
	for i, subsection := range *s.Subsections {
		if subsection.ID == id {
			*s.Subsections = append((*s.Subsections)[:i], (*s.Subsections)[i+1:]...)
			return nil
		}
		err = removeNodeFromTree(subsection, id)
		if err == nil {
			return nil
		}
	}
	for i, document := range *s.Documents {
		if document.ID == id {
			*s.Documents = append((*s.Documents)[:i], (*s.Documents)[i+1:]...)
			return nil
		}
	}
	for i, resource := range *s.Resources {
		if resource.ID == id {
			*s.Resources = append((*s.Resources)[:i], (*s.Resources)[i+1:]...)
			return nil
		}
	}
	return err
}
