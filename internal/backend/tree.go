package backend

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	mutexSync "sync"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
	"github.com/OneOfOne/xxhash"
)

const (
	TypeSection  = "section"
	TypeDocument = "document"
	TypeResource = "resource"

	// RootSectionID is the id of the root section of the tree
	RootSectionID = "root"

	markdownFileExtension = ".md"
)

var (
	// ErrInvalidName is returned when a section, document or resource name is not a plain file name
	ErrInvalidName = errors.New("invalid name")
	// ErrAlreadyExists is returned when the target of a create or rename operation already exists
	ErrAlreadyExists = errors.New("target already exists")
	// ErrNotFound is returned when an item does not exist
	ErrNotFound = errors.New("item not found")
)

type (
	Section struct {
		Type        string       `json:"type" xml:"type" form:"type" query:"type"`
		ID          string       `json:"id" xml:"id" form:"id" query:"id"`
		Name        string       `json:"name" xml:"name" form:"name" query:"name"`
		Path        string       `json:"-" xml:"-" form:"-" query:"-"`
		RelPath     string       `json:"path" xml:"path" form:"path" query:"path"`
		Index       string       `json:"index" xml:"index" form:"index" query:"index"`
		Subsections *[]*Section  `json:"subsections" xml:"subsections" form:"subsections" query:"subsections"`
		Documents   *[]*Document `json:"documents" xml:"documents" form:"documents" query:"documents"`
		Resources   *[]*Resource `json:"resources" xml:"resources" form:"resources" query:"resources"`
	}

	Document struct {
		Type     string    `json:"type" xml:"type" form:"type" query:"type"`
		ID       string    `json:"id" xml:"id" form:"id" query:"id"`
		Name     string    `json:"name" xml:"name" form:"name" query:"name"`
		Path     string    `json:"-" xml:"-" form:"-" query:"-"`
		RelPath  string    `json:"path" xml:"path" form:"path" query:"path"`
		Filesize int64     `json:"filesize" xml:"filesize" form:"filesize" query:"filesize"`
		ModTime  time.Time `json:"modtime" xml:"modtime" form:"modtime" query:"modtime"`
		SubUrl   string    `json:"url" xml:"url" form:"url" query:"url"`
	}

	Resource struct {
		Type     string    `json:"type" xml:"type" form:"type" query:"type"`
		ID       string    `json:"id" xml:"id" form:"id" query:"id"`
		Name     string    `json:"name" xml:"name" form:"name" query:"name"`
		Path     string    `json:"-" xml:"-" form:"-" query:"-"`
		RelPath  string    `json:"path" xml:"path" form:"path" query:"path"`
		Filesize int64     `json:"filesize" xml:"filesize" form:"filesize" query:"filesize"`
		ModTime  time.Time `json:"modtime" xml:"modtime" form:"modtime" query:"modtime"`
		MimeType string    `json:"mimeType" xml:"mimeType" form:"mimeType" query:"mimeType"`
		Kind     string    `json:"kind" xml:"kind" form:"kind" query:"kind"`
	}
)

// TreeManager keeps an in memory representation of the file structure of the docs folder.
//
// The tree is an immutable snapshot: every change on disk (made through the TreeManager or
// detected by the file watcher) builds a new tree and swaps it in. Items returned by the
// getters never change after they have been returned and can be used without locking.
type TreeManager struct {
	// lock guards the tree pointer
	lock mutexSync.RWMutex
	// fsLock serializes modifications of the docs folder made through the TreeManager
	fsLock mutexSync.Mutex
	// buildLock serializes tree rebuilds so listeners see them in order
	buildLock mutexSync.Mutex

	rootPath string
	tree     *Section

	listenerLock mutexSync.Mutex
	listeners    []func(oldTree *Section, newTree *Section)
}

// NewTreeManager creates a TreeManager for the configured docs path and builds the initial tree
func NewTreeManager() *TreeManager {
	return NewTreeManagerForPath(configuration.CurrentConfig.MkDocs.DocsPath)
}

// NewTreeManagerForPath creates a TreeManager for the given docs path and builds the initial tree
func NewTreeManagerForPath(rootPath string) *TreeManager {
	treeManager := &TreeManager{
		rootPath: filepath.Clean(rootPath),
	}
	treeManager.CreateItemTree()
	return treeManager
}

// RootPath returns the path of the docs folder
func (tm *TreeManager) RootPath() string {
	return tm.rootPath
}

// AddTreeChangeListener registers a function that is called after the tree has been rebuilt.
// It is called synchronously, in the order of the rebuilds.
func (tm *TreeManager) AddTreeChangeListener(f func(oldTree *Section, newTree *Section)) {
	tm.listenerLock.Lock()
	defer tm.listenerLock.Unlock()
	tm.listeners = append(tm.listeners, f)
}

// Root returns the current snapshot of the tree
func (tm *TreeManager) Root() *Section {
	tm.lock.RLock()
	defer tm.lock.RUnlock()
	return tm.tree
}

// CreateItemTree traverses the docs directory and puts all files into a new tree snapshot
func (tm *TreeManager) CreateItemTree() {
	tm.buildLock.Lock()
	defer tm.buildLock.Unlock()

	ignore := newIgnoreMatcher(tm.rootPath)
	parent, name := filepath.Split(tm.rootPath)
	root := tm.createSectionForTree(parent, name, RootSectionID)
	tm.populateItemTree(root, root.Path, ignore)
	setSectionIndexes(root)

	tm.lock.Lock()
	oldTree := tm.tree
	tm.tree = root
	tm.lock.Unlock()

	tm.listenerLock.Lock()
	listeners := slices.Clone(tm.listeners)
	tm.listenerLock.Unlock()
	for _, l := range listeners {
		l(oldTree, root)
	}
}

// recursive function that creates a subtree of the complete item tree
func (tm *TreeManager) populateItemTree(section *Section, path string, ignore func(string) bool) {
	files, err := os.ReadDir(path)
	if err != nil {
		log.Printf("Unable to read directory %s: %v", path, err)
		return
	}

	for _, f := range files {
		totalPath := filepath.Join(path, f.Name())
		if ignore(totalPath) {
			continue
		}

		if f.IsDir() {
			subSection := tm.createSectionForTree(path, f.Name(), "")
			*section.Subsections = append(*section.Subsections, subSection)
			tm.populateItemTree(subSection, subSection.Path, ignore)
			continue
		}

		info, err := f.Info()
		if err != nil {
			// the file was removed while reading the directory
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if strings.HasSuffix(info.Name(), markdownFileExtension) {
			*section.Documents = append(*section.Documents, tm.createDocumentForTree(path, info))
		} else {
			*section.Resources = append(*section.Resources, tm.createResourceForTree(path, info))
		}
	}
}

// setSectionIndexes sets the index document id of every section
func setSectionIndexes(s *Section) {
	if d := sectionIndexDocument(s); d != nil {
		s.Index = d.ID
	}
	for _, sub := range *s.Subsections {
		setSectionIndexes(sub)
	}
}

// relPath returns the path of an item relative to the docs folder, slash separated
func (tm *TreeManager) relPath(absPath string) string {
	rel, err := filepath.Rel(tm.rootPath, absPath)
	if err != nil || rel == "." {
		return ""
	}
	return filepath.ToSlash(rel)
}

// newIgnoreMatcher returns a function that reports whether a path should be left out of the tree.
// Hidden files and folders, the extra_css files of mkdocs.yml and the configured blacklist are ignored.
func newIgnoreMatcher(rootPath string) func(path string) bool {
	var extraCss []string
	if mkDocsConfig, err := readMkDocsConfig(); err == nil {
		extraCss = mkDocsConfig.ExtraCss
	}
	blacklist := configuration.CurrentConfig.MkDocs.Blacklist

	return func(path string) bool {
		if strings.HasPrefix(filepath.Base(path), ".") {
			return true
		}
		rel, err := filepath.Rel(rootPath, path)
		if err != nil {
			return false
		}
		rel = filepath.ToSlash(rel)
		return slices.Contains(extraCss, rel) || slices.Contains(blacklist, rel)
	}
}

// creates a (non-cryptographic) hash of the given string
func createHash(s string) string {
	return strconv.FormatUint(xxhash.ChecksumString64(s), 10)
}

// GenerateId generates the item id for the given path
func (tm *TreeManager) GenerateId(path string) string {
	if filepath.Clean(path) == tm.rootPath {
		return RootSectionID
	}
	return createHash(path)
}

// creates a section object for storing in the tree
func (tm *TreeManager) createSectionForTree(path string, name string, id string) *Section {
	sectionPath := filepath.Join(path, name)
	if id == "" {
		id = tm.GenerateId(sectionPath)
	}
	return &Section{
		Type:        TypeSection,
		ID:          id,
		Name:        name,
		Path:        sectionPath,
		RelPath:     tm.relPath(sectionPath),
		Documents:   &[]*Document{},
		Subsections: &[]*Section{},
		Resources:   &[]*Resource{},
	}
}

// creates a document object for storing in the tree
func (tm *TreeManager) createDocumentForTree(parentFolderPath string, f os.FileInfo) *Document {
	fileName := f.Name()
	documentPath := filepath.Join(parentFolderPath, fileName)

	subUrl := ""
	for _, element := range strings.Split(strings.TrimPrefix(documentPath, tm.rootPath), string(filepath.Separator)) {
		element = strings.TrimSuffix(element, markdownFileExtension)
		if len(element) > 0 {
			subUrl += url.PathEscape(element) + "/"
		}
	}

	return &Document{
		Type:     TypeDocument,
		ID:       tm.GenerateId(documentPath),
		Name:     strings.TrimSuffix(fileName, markdownFileExtension),
		Path:     documentPath,
		RelPath:  tm.relPath(documentPath),
		Filesize: f.Size(),
		ModTime:  f.ModTime(),
		SubUrl:   subUrl,
	}
}

// creates a resource object for storing in the tree
func (tm *TreeManager) createResourceForTree(parentFolderPath string, f os.FileInfo) *Resource {
	resourcePath := filepath.Join(parentFolderPath, f.Name())
	mimeType := mimeTypeOf(f.Name())
	return &Resource{
		Type:     TypeResource,
		ID:       tm.GenerateId(resourcePath),
		Name:     f.Name(),
		Path:     resourcePath,
		RelPath:  tm.relPath(resourcePath),
		Filesize: f.Size(),
		ModTime:  f.ModTime(),
		MimeType: mimeType,
		Kind:     resourceKind(mimeType),
	}
}

// GetSection finds a section with the given id in the document tree
func (tm *TreeManager) GetSection(id string) *Section {
	return findSectionRecursive(tm.Root(), id)
}

// GetDocument finds a document with the given id in the document tree
func (tm *TreeManager) GetDocument(id string) *Document {
	return findDocumentRecursive(tm.Root(), id)
}

// GetResource finds a resource with the given id in the document tree
func (tm *TreeManager) GetResource(id string) *Resource {
	return findResourceRecursive(tm.Root(), id)
}

// ReadDocumentContent reads the current content of the given document from disk
func (tm *TreeManager) ReadDocumentContent(d *Document) (string, error) {
	data, err := os.ReadFile(d.Path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// traverses the tree and searches for a section with the given id
func findSectionRecursive(section *Section, id string) *Section {
	if section == nil {
		return nil
	}
	if section.ID == id {
		return section
	}
	for _, subsection := range *section.Subsections {
		if s := findSectionRecursive(subsection, id); s != nil {
			return s
		}
	}
	return nil
}

// traverses the tree and searches for a document with the given id
func findDocumentRecursive(section *Section, id string) *Document {
	if section == nil {
		return nil
	}
	for _, document := range *section.Documents {
		if document.ID == id {
			return document
		}
	}
	for _, subsection := range *section.Subsections {
		if d := findDocumentRecursive(subsection, id); d != nil {
			return d
		}
	}
	return nil
}

// traverses the tree and searches for a resource with the given id
func findResourceRecursive(section *Section, id string) *Resource {
	if section == nil {
		return nil
	}
	for _, resource := range *section.Resources {
		if resource.ID == id {
			return resource
		}
	}
	for _, subsection := range *section.Subsections {
		if r := findResourceRecursive(subsection, id); r != nil {
			return r
		}
	}
	return nil
}

// ValidateName checks that a name given by a client is a plain, visible file name
// that cannot escape its parent folder.
func ValidateName(name string) error {
	if name == "" || strings.ContainsAny(name, "/\\\x00") || strings.HasPrefix(name, ".") {
		return fmt.Errorf("%w: %q", ErrInvalidName, name)
	}
	return nil
}

func fileExists(filePath string) (bool, error) {
	_, err := os.Stat(filePath)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// prepares the path of a new child item of the given parent section
func (tm *TreeManager) newChildPath(parentSectionId string, fileName string) (string, error) {
	if err := ValidateName(fileName); err != nil {
		return "", err
	}
	parent := tm.GetSection(parentSectionId)
	if parent == nil {
		return "", fmt.Errorf("%w: parent section %s", ErrNotFound, parentSectionId)
	}
	filePath := filepath.Join(parent.Path, fileName)
	exists, err := fileExists(filePath)
	if err != nil {
		return "", err
	}
	if exists {
		return "", fmt.Errorf("%w: %s", ErrAlreadyExists, fileName)
	}
	return filePath, nil
}

// CreateSection creates a new section with the given name as a child of the given parent section
func (tm *TreeManager) CreateSection(parentSectionId string, sectionName string) (*Section, error) {
	tm.fsLock.Lock()
	defer tm.fsLock.Unlock()

	sectionPath, err := tm.newChildPath(parentSectionId, sectionName)
	if err != nil {
		return nil, err
	}
	if err = os.Mkdir(sectionPath, 0o755); err != nil {
		return nil, err
	}
	tm.CreateItemTree()
	return tm.GetSection(tm.GenerateId(sectionPath)), nil
}

// CreateDocument creates a new, empty document as a child of the given parent section
func (tm *TreeManager) CreateDocument(parentSectionId string, documentName string) (*Document, error) {
	tm.fsLock.Lock()
	defer tm.fsLock.Unlock()

	if err := ValidateName(documentName); err != nil {
		return nil, err
	}
	documentPath, err := tm.newChildPath(parentSectionId, documentName+markdownFileExtension)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(documentPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	tm.CreateItemTree()
	return tm.GetDocument(tm.GenerateId(documentPath)), nil
}

// CreateResource creates a new resource file with the given content as a child of the given parent section
func (tm *TreeManager) CreateResource(parentSectionId string, resourceName string, content io.Reader) (*Resource, error) {
	tm.fsLock.Lock()
	defer tm.fsLock.Unlock()

	resourcePath, err := tm.newChildPath(parentSectionId, resourceName)
	if err != nil {
		return nil, err
	}
	dst, err := os.OpenFile(resourcePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	_, err = io.Copy(dst, content)
	closeErr := dst.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(resourcePath)
		return nil, err
	}
	tm.CreateItemTree()
	return tm.GetResource(tm.GenerateId(resourcePath)), nil
}

// renames the item at oldPath to the new file name in the same folder
func (tm *TreeManager) renamePath(oldPath string, newFileName string) (string, error) {
	if err := ValidateName(newFileName); err != nil {
		return "", err
	}
	newPath := filepath.Join(filepath.Dir(oldPath), newFileName)
	if newPath == oldPath {
		return newPath, nil
	}
	exists, err := fileExists(newPath)
	if err != nil {
		return "", err
	}
	if exists {
		return "", fmt.Errorf("%w: %s", ErrAlreadyExists, newFileName)
	}
	if err = os.Rename(oldPath, newPath); err != nil {
		return "", err
	}
	tm.CreateItemTree()
	return newPath, nil
}

// RenameSection renames the folder of the given section
func (tm *TreeManager) RenameSection(section *Section, name string) (*Section, error) {
	tm.fsLock.Lock()
	defer tm.fsLock.Unlock()
	if section.ID == RootSectionID {
		return nil, fmt.Errorf("%w: the root section cannot be renamed", ErrInvalidName)
	}
	newPath, err := tm.renamePath(section.Path, name)
	if err != nil {
		return nil, err
	}
	return tm.GetSection(tm.GenerateId(newPath)), nil
}

// RenameDocument renames the file of the given document (name without the .md extension)
func (tm *TreeManager) RenameDocument(document *Document, name string) (*Document, error) {
	tm.fsLock.Lock()
	defer tm.fsLock.Unlock()
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	newPath, err := tm.renamePath(document.Path, name+markdownFileExtension)
	if err != nil {
		return nil, err
	}
	return tm.GetDocument(tm.GenerateId(newPath)), nil
}

// RenameResource renames the file of the given resource
func (tm *TreeManager) RenameResource(resource *Resource, name string) (*Resource, error) {
	tm.fsLock.Lock()
	defer tm.fsLock.Unlock()
	newPath, err := tm.renamePath(resource.Path, name)
	if err != nil {
		return nil, err
	}
	return tm.GetResource(tm.GenerateId(newPath)), nil
}

// DeleteItem deletes a file/folder with the given ID and type from disk.
// It returns false if no item with the given id and type exists.
func (tm *TreeManager) DeleteItem(id string, itemType string) (bool, error) {
	tm.fsLock.Lock()
	defer tm.fsLock.Unlock()

	var path string
	switch itemType {
	case TypeSection:
		if id == RootSectionID {
			return false, fmt.Errorf("%w: the root section cannot be deleted", ErrInvalidName)
		}
		if s := tm.GetSection(id); s != nil {
			path = s.Path
		}
	case TypeDocument:
		if d := tm.GetDocument(id); d != nil {
			path = d.Path
		}
	case TypeResource:
		if r := tm.GetResource(id); r != nil {
			path = r.Path
		}
	}
	if path == "" {
		return false, nil
	}

	if err := os.RemoveAll(path); err != nil {
		return false, err
	}
	tm.CreateItemTree()
	return true, nil
}
