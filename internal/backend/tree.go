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
	mutexSync "sync"
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

type TreeManager struct {
	lock mutexSync.RWMutex

	rootPath string
	// DocumentTree an in memory representation of the mkdocs file structure
	DocumentTree Section
}

func NewTreeManager() *TreeManager {
	rootPath := configuration.CurrentConfig.MkDocs.DocsPath
	treeManager := &TreeManager{
		rootPath: rootPath,
	}
	treeManager.CreateItemTree()
	return treeManager
}

// CreateItemTree traverses the mkdocs directory and puts all files into a tree representation
func (tm *TreeManager) CreateItemTree() {
	tm.lock.Lock()
	defer tm.lock.Unlock()
	path, file := filepath.Split(tm.rootPath)

	tm.DocumentTree = tm.createSectionForTree(path, file, "root")
	searchDir := tm.DocumentTree.Path
	tm.populateItemTree(&tm.DocumentTree, searchDir)
}

// recursive function that creates a subtree of the complete item tree
func (tm *TreeManager) populateItemTree(section *Section, path string) {
	files, err := os.ReadDir(path)
	if err != nil {
		log.Fatal(err)
	}

	for _, f := range files {
		totalPath := filepath.Join(path, f.Name())
		if tm.isOnIgnoreList(totalPath) {
			continue
		}

		if f.IsDir() {
			var subSection = tm.createSectionForTree(path, f.Name(), "")
			*section.Subsections = append(*section.Subsections, &subSection)

			// recursive call
			tm.populateItemTree(&subSection, filepath.Join(path, subSection.Name))
		} else {
			f, _ := f.Info()
			if strings.HasSuffix(f.Name(), markdownFileExtension) {
				var document = tm.createDocumentForTree(path, f)
				*section.Documents = append(*section.Documents, &document)
			} else {
				var resource = tm.createResourceForTree(path, f)
				*section.Resources = append(*section.Resources, &resource)
			}
		}
	}
}

func (tm *TreeManager) isOnIgnoreList(path string) bool {
	mkDocsConfig, err := readMkDocsConfig()
	if err != nil {
		return false
	}

	pathInRootPath := strings.TrimPrefix(path, tm.rootPath)
	pathInRootPath = strings.TrimPrefix(pathInRootPath, "/")

	shouldIgnore := slices.Contains(mkDocsConfig.ExtraCss, pathInRootPath)
	shouldIgnore = shouldIgnore || slices.Contains(configuration.CurrentConfig.MkDocs.Blacklist, pathInRootPath)

	return shouldIgnore
}

// creates a (non-cryptographic) hash of the given string
func (tm *TreeManager) createHash(s string) string {
	pathHash := xxhash.ChecksumString64(s)
	return strconv.FormatUint(pathHash, 10)
}

// creates a section object for storing in the tree
func (tm *TreeManager) createSectionForTree(path string, name string, id string) Section {
	sectionPath := filepath.Join(path, name)

	if id == "" {
		id = tm.generateId(sectionPath)
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
func (tm *TreeManager) generateId(path string) string {
	return tm.createHash(path)
}

// creates a document object for storing in the tree
func (tm *TreeManager) createDocumentForTree(parentFolderPath string, f os.FileInfo) (document Document) {
	fileName := f.Name()
	fileSize := f.Size()
	fileModTime := f.ModTime()
	documentPath := filepath.Join(parentFolderPath, f.Name())

	subUrl := ""
	for _, element := range strings.Split(strings.TrimPrefix(documentPath, tm.rootPath), string(filepath.Separator)) {
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
		ID:       tm.generateId(documentPath),
		Name:     fileName[0 : len(fileName)-len(markdownFileExtension)],
		Path:     documentPath,
		Filesize: fileSize,
		ModTime:  fileModTime,
		Content:  content,
		SubUrl:   subUrl,
	}
}

// creates a resource object for storing in the tree
func (tm *TreeManager) createResourceForTree(parentFolderPath string, f os.FileInfo) Resource {
	fileName := f.Name()
	fileSize := f.Size()
	fileModTime := f.ModTime()
	resourcePath := filepath.Join(parentFolderPath, f.Name())

	return Resource{
		Type:     TypeResource,
		ID:       tm.generateId(resourcePath),
		Name:     fileName,
		Path:     resourcePath,
		Filesize: fileSize,
		ModTime:  fileModTime,
	}
}

// GetSection finds a document with the given id in the document tree
func (tm *TreeManager) GetSection(id string) *Section {
	tm.lock.Lock()
	defer tm.lock.Unlock()
	return tm.findSectionRecursive(&tm.DocumentTree, id)
}

// GetDocument finds a document with the given id in the document tree
func (tm *TreeManager) GetDocument(id string) *Document {
	tm.lock.Lock()
	defer tm.lock.Unlock()
	return tm.findDocumentRecursive(&tm.DocumentTree, id)
}

// GetResource finds a resource with the given id in the document tree
func (tm *TreeManager) GetResource(id string) *Resource {
	tm.lock.Lock()
	defer tm.lock.Unlock()
	return tm.findResourceRecursive(&tm.DocumentTree, id)
}

func (tm *TreeManager) CreateResource(parentSectionId string, resourceName string, content string) (resource *Resource, err error) {
	tm.lock.Lock()
	defer tm.lock.Unlock()
	parent := tm.findSectionRecursive(&tm.DocumentTree, parentSectionId)

	if parent == nil {
		return nil, errors.New("Parent section " + parentSectionId + " does not exist")
	}

	var fileName = resourceName
	filePath := filepath.Join(parent.Path, fileName)

	exists, err := tm.fileExists(filePath)
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

	newResourceTreeItem := tm.createResourceForTree(parent.Path, fileInfo)
	*parent.Resources = append(*parent.Resources, &newResourceTreeItem)

	return &newResourceTreeItem, err
}

func (tm *TreeManager) CreateResourceFromMultipart(parentSectionId string, resourceName string, src multipart.File) (resource *Resource, err error) {
	tm.lock.Lock()
	defer tm.lock.Unlock()
	defer src.Close()

	parent := tm.findSectionRecursive(&tm.DocumentTree, parentSectionId)

	if parent == nil {
		return nil, errors.New("Parent section " + parentSectionId + " does not exist")
	}

	var fileName = resourceName

	filePath := filepath.Join(parent.Path, fileName)

	exists, err := tm.fileExists(filePath)
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

	newResourceTreeItem := tm.createResourceForTree(parent.Path, fileInfo)
	*parent.Resources = append(*parent.Resources, &newResourceTreeItem)

	return &newResourceTreeItem, err
}

// traverses the tree and searches for a document with the given id
func (tm *TreeManager) findSectionRecursive(section *Section, id string) *Section {
	if section.ID == id {
		return section
	}

	for _, subsection := range *section.Subsections {
		d := tm.findSectionRecursive(subsection, id)
		if d != nil {
			return d
		}
	}

	return nil
}

// traverses the tree and searches for a document with the given id
func (tm *TreeManager) findDocumentRecursive(section *Section, id string) *Document {
	for _, document := range *section.Documents {
		if document.ID == id {
			return document
		}
	}

	for _, subsection := range *section.Subsections {
		d := tm.findDocumentRecursive(subsection, id)
		if d != nil {
			return d
		}
	}

	return nil
}

// traverses the tree and searches for a resource with the given id
func (tm *TreeManager) findResourceRecursive(section *Section, id string) *Resource {
	for _, resource := range *section.Resources {
		if resource.ID == id {
			return resource
		}
	}

	for _, subsection := range *section.Subsections {
		r := tm.findResourceRecursive(subsection, id)
		if r != nil {
			return r
		}
	}

	return nil
}

// CreateSection creates a new section with the given name as a child of the given parent section
func (tm *TreeManager) CreateSection(parentSection *Section, sectionName string) (section *Section, err error) {
	tm.lock.Lock()
	defer tm.lock.Unlock()
	newSection := tm.createSectionForTree(parentSection.Path, sectionName, "")

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
func (tm *TreeManager) CreateDocument(parentSectionId string, documentName string) (document *Document, err error) {
	tm.lock.Lock()
	defer tm.lock.Unlock()
	parent := tm.findSectionRecursive(&tm.DocumentTree, parentSectionId)

	if parent == nil {
		return nil, errors.New("Parent section " + parentSectionId + " does not exist")
	}

	var fileName = documentName + markdownFileExtension

	filePath := filepath.Join(parent.Path, fileName)

	exists, err := tm.fileExists(filePath)
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

	newDocumentTreeItem := tm.createDocumentForTree(parent.Path, fileInfo)
	*parent.Documents = append(*parent.Documents, &newDocumentTreeItem)

	return &newDocumentTreeItem, err
}

func (tm *TreeManager) RenameSection(section *Section, name string) (sec *Section, err error) {
	tm.lock.Lock()
	defer tm.lock.Unlock()
	var newFilePath = filepath.Join(filepath.Dir(section.Path), name)
	exists, err := tm.fileExists(newFilePath)
	if exists {
		return nil, errors.New("Target section " + section.Name + " already exists!")
	}

	err = os.Rename(section.Path, newFilePath)
	if err != nil {
		return nil, err
	}

	tm.CreateItemTree()
	section = tm.GetSection(tm.generateId(newFilePath))

	return section, err
}

func (tm *TreeManager) RenameDocument(document *Document, name string) (doc *Document, err error) {
	tm.lock.Lock()
	defer tm.lock.Unlock()
	var fileName = name + markdownFileExtension
	var newFilePath = filepath.Join(filepath.Dir(document.Path), fileName)

	exists, err := tm.fileExists(newFilePath)
	if exists {
		return nil, errors.New("Target document " + document.Name + " already exists!")
	}

	err = os.Rename(document.Path, newFilePath)
	if err != nil {
		return nil, err
	}

	tm.CreateItemTree()
	document = tm.GetDocument(tm.generateId(newFilePath))

	return document, err
}

func (tm *TreeManager) RenameResource(resource *Resource, name string) (res *Resource, err error) {
	tm.lock.Lock()
	defer tm.lock.Unlock()
	var newFilePath = filepath.Join(filepath.Dir(resource.Path), name)
	exists, err := tm.fileExists(newFilePath)
	if exists {
		return nil, errors.New("Target resource " + resource.Name + " already exists!")
	}

	err = os.Rename(resource.Path, newFilePath)
	if err != nil {
		return nil, err
	}

	tm.CreateItemTree()
	resource = tm.GetResource(tm.generateId(newFilePath))

	return resource, err
}

func (tm *TreeManager) fileExists(filePath string) (exists bool, err error) {
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
func (tm *TreeManager) DeleteItem(id string, itemType string) (success bool, err error) {
	tm.lock.Lock()
	defer tm.lock.Unlock()
	var path string
	switch itemType {
	case TypeSection:
		s := tm.findSectionRecursive(&tm.DocumentTree, id)
		if s != nil {
			path = s.Path
		} else {
			return false, nil
		}
	case TypeDocument:
		d := tm.findDocumentRecursive(&tm.DocumentTree, id)
		if d != nil {
			path = d.Path
		} else {
			return false, nil
		}
	case TypeResource:
		r := tm.findResourceRecursive(&tm.DocumentTree, id)
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

	err = tm.removeNodeFromTree(&tm.DocumentTree, id)

	return success, err
}

func (tm *TreeManager) removeNodeFromTree(s *Section, id string) (err error) {
	for i, subsection := range *s.Subsections {
		if subsection.ID == id {
			*s.Subsections = append((*s.Subsections)[:i], (*s.Subsections)[i+1:]...)
			return nil
		}
		err = tm.removeNodeFromTree(subsection, id)
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
