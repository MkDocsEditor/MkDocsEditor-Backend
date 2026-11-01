package backend

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
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
		Type        string      `json:"type"`
		ID          string      `json:"id"`
		Name        string      `json:"name"`
		Path        string      `json:"-"`
		RelPath     string      `json:"path"`
		Index       string      `json:"index"`
		Subsections []*Section  `json:"subsections"`
		Documents   []*Document `json:"documents"`
		Resources   []*Resource `json:"resources"`
	}

	Document struct {
		Type     string    `json:"type"`
		ID       string    `json:"id"`
		Name     string    `json:"name"`
		Path     string    `json:"-"`
		RelPath  string    `json:"path"`
		Filesize int64     `json:"filesize"`
		ModTime  time.Time `json:"modtime"`
		SubURL   string    `json:"url"`
	}

	Resource struct {
		Type     string    `json:"type"`
		ID       string    `json:"id"`
		Name     string    `json:"name"`
		Path     string    `json:"-"`
		RelPath  string    `json:"path"`
		Filesize int64     `json:"filesize"`
		ModTime  time.Time `json:"modtime"`
		MimeType string    `json:"mimeType"`
		Kind     string    `json:"kind"`
		// Thumbnail is true when GET /resource/{id}/thumbnail/ works for the resource
		Thumbnail bool `json:"thumbnail"`
	}
)

// walk iterates over the section and all sections below it, parents first. A nil section is empty.
func (s *Section) walk() iter.Seq[*Section] {
	return func(yield func(*Section) bool) {
		s.visit(yield)
	}
}

func (s *Section) visit(yield func(*Section) bool) bool {
	if s == nil {
		return true
	}
	if !yield(s) {
		return false
	}
	for _, sub := range s.Subsections {
		if !sub.visit(yield) {
			return false
		}
	}
	return true
}

// TreeManager keeps an in memory representation of the file structure of the docs folder.
//
// The tree is an immutable snapshot: every change on disk (made through the TreeManager or
// detected by the file watcher) builds a new tree and swaps it in. Items returned by the
// getters never change after they have been returned and can be used without locking.
type TreeManager struct {
	// lock guards the tree pointer
	lock sync.RWMutex
	// fsLock serializes modifications of the docs folder made through the TreeManager
	fsLock sync.Mutex
	// buildLock serializes tree rebuilds so listeners see them in order
	buildLock sync.Mutex

	rootPath string
	mkdocs   configuration.MkDocsConfiguration
	tree     *Section

	listenerLock sync.Mutex
	listeners    []func(oldTree *Section, newTree *Section)
}

// NewTreeManager creates a TreeManager for the configured docs path and builds the initial tree
func NewTreeManager(mkdocs configuration.MkDocsConfiguration) *TreeManager {
	treeManager := &TreeManager{
		rootPath: filepath.Clean(mkdocs.DocsPath),
		mkdocs:   mkdocs,
	}
	treeManager.CreateItemTree()
	return treeManager
}

// RootPath returns the path of the docs folder
func (tm *TreeManager) RootPath() string {
	return tm.rootPath
}

// ConfigFile returns the path of mkdocs.yml
func (tm *TreeManager) ConfigFile() string {
	return tm.mkdocs.ConfigFile
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

	ignore := tm.newIgnoreMatcher()
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
			section.Subsections = append(section.Subsections, subSection)
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
			section.Documents = append(section.Documents, tm.createDocumentForTree(path, info))
		} else {
			section.Resources = append(section.Resources, tm.createResourceForTree(path, info))
		}
	}
}

// setSectionIndexes sets the index document id of every section
func setSectionIndexes(s *Section) {
	for section := range s.walk() {
		if d := sectionIndexDocument(section); d != nil {
			section.Index = d.ID
		}
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
func (tm *TreeManager) newIgnoreMatcher() func(path string) bool {
	blocked := tm.blockedPaths()
	return func(path string) bool {
		if strings.HasPrefix(filepath.Base(path), ".") {
			return true
		}
		rel, err := filepath.Rel(tm.rootPath, path)
		if err != nil {
			return false
		}
		return slices.Contains(blocked, filepath.ToSlash(rel))
	}
}

// blockedPaths are the paths that are no items: the blacklist and the extra_css files
func (tm *TreeManager) blockedPaths() []string {
	blocked := slices.Clone(tm.mkdocs.Blacklist)
	if extraCSS, err := readExtraCSS(tm.mkdocs.ConfigFile); err == nil {
		blocked = append(blocked, extraCSS...)
	}
	return blocked
}

// creates a (non-cryptographic) hash of the given string
func createHash(s string) string {
	return strconv.FormatUint(xxhash.ChecksumString64(s), 10)
}

// GenerateID generates the item id for the given path
func (tm *TreeManager) GenerateID(path string) string {
	if filepath.Clean(path) == tm.rootPath {
		return RootSectionID
	}
	return createHash(path)
}

// idOfRelPath returns the item id of a path relative to the docs folder
func (tm *TreeManager) idOfRelPath(rel string) string {
	return tm.GenerateID(filepath.Join(tm.rootPath, filepath.FromSlash(rel)))
}

// creates a section object for storing in the tree
func (tm *TreeManager) createSectionForTree(path string, name string, id string) *Section {
	sectionPath := filepath.Join(path, name)
	if id == "" {
		id = tm.GenerateID(sectionPath)
	}
	return &Section{
		Type:        TypeSection,
		ID:          id,
		Name:        name,
		Path:        sectionPath,
		RelPath:     tm.relPath(sectionPath),
		Documents:   []*Document{},
		Subsections: []*Section{},
		Resources:   []*Resource{},
	}
}

// creates a document object for storing in the tree
func (tm *TreeManager) createDocumentForTree(parentFolderPath string, f os.FileInfo) *Document {
	fileName := f.Name()
	documentPath := filepath.Join(parentFolderPath, fileName)

	relPath := tm.relPath(documentPath)
	var subURL strings.Builder
	for element := range strings.SplitSeq(strings.TrimSuffix(relPath, markdownFileExtension), "/") {
		subURL.WriteString(url.PathEscape(element) + "/")
	}

	return &Document{
		Type:     TypeDocument,
		ID:       tm.GenerateID(documentPath),
		Name:     strings.TrimSuffix(fileName, markdownFileExtension),
		Path:     documentPath,
		RelPath:  relPath,
		Filesize: f.Size(),
		ModTime:  f.ModTime(),
		SubURL:   subURL.String(),
	}
}

// creates a resource object for storing in the tree
func (tm *TreeManager) createResourceForTree(parentFolderPath string, f os.FileInfo) *Resource {
	resourcePath := filepath.Join(parentFolderPath, f.Name())
	mimeType := mimeTypeOf(f.Name())
	return &Resource{
		Type:      TypeResource,
		ID:        tm.GenerateID(resourcePath),
		Name:      f.Name(),
		Path:      resourcePath,
		RelPath:   tm.relPath(resourcePath),
		Filesize:  f.Size(),
		ModTime:   f.ModTime(),
		MimeType:  mimeType,
		Kind:      resourceKind(mimeType),
		Thumbnail: hasThumbnail(mimeType),
	}
}

// GetSection finds a section with the given id in the document tree
func (tm *TreeManager) GetSection(id string) *Section {
	for s := range tm.Root().walk() {
		if s.ID == id {
			return s
		}
	}
	return nil
}

// GetDocument finds a document with the given id in the document tree
func (tm *TreeManager) GetDocument(id string) *Document {
	for s := range tm.Root().walk() {
		if i := slices.IndexFunc(s.Documents, func(d *Document) bool { return d.ID == id }); i >= 0 {
			return s.Documents[i]
		}
	}
	return nil
}

// GetResource finds a resource with the given id in the document tree
func (tm *TreeManager) GetResource(id string) *Resource {
	for s := range tm.Root().walk() {
		if i := slices.IndexFunc(s.Resources, func(r *Resource) bool { return r.ID == id }); i >= 0 {
			return s.Resources[i]
		}
	}
	return nil
}

// ReadDocumentContent reads the current content of the given document from disk
func (tm *TreeManager) ReadDocumentContent(d *Document) (string, error) {
	data, err := os.ReadFile(d.Path)
	if err != nil {
		return "", err
	}
	return string(data), nil
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
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// prepares the path of a new child item of the given parent section
func (tm *TreeManager) newChildPath(parentSectionID string, fileName string) (string, error) {
	if err := ValidateName(fileName); err != nil {
		return "", err
	}
	parent := tm.GetSection(parentSectionID)
	if parent == nil {
		return "", fmt.Errorf("%w: parent section %s", ErrNotFound, parentSectionID)
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
func (tm *TreeManager) CreateSection(parentSectionID string, sectionName string) (*Section, error) {
	tm.fsLock.Lock()
	defer tm.fsLock.Unlock()

	sectionPath, err := tm.newChildPath(parentSectionID, sectionName)
	if err != nil {
		return nil, err
	}
	if err = os.Mkdir(sectionPath, 0o755); err != nil {
		return nil, err
	}
	tm.CreateItemTree()
	return tm.GetSection(tm.GenerateID(sectionPath)), nil
}

// CreateDocument creates a new, empty document as a child of the given parent section
func (tm *TreeManager) CreateDocument(parentSectionID string, documentName string) (*Document, error) {
	tm.fsLock.Lock()
	defer tm.fsLock.Unlock()

	if err := ValidateName(documentName); err != nil {
		return nil, err
	}
	documentPath, err := tm.newChildPath(parentSectionID, documentName+markdownFileExtension)
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
	return tm.GetDocument(tm.GenerateID(documentPath)), nil
}

// CreateResource creates a new resource file with the given content as a child of the given parent section
func (tm *TreeManager) CreateResource(parentSectionID string, resourceName string, content io.Reader) (*Resource, error) {
	tm.fsLock.Lock()
	defer tm.fsLock.Unlock()

	resourcePath, err := tm.newChildPath(parentSectionID, resourceName)
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
	return tm.GetResource(tm.GenerateID(resourcePath)), nil
}

// itemPath returns the path of the item with the given id and type, or "" if there is none
func (tm *TreeManager) itemPath(id string, itemType string) string {
	switch itemType {
	case TypeSection:
		if s := tm.GetSection(id); s != nil {
			return s.Path
		}
	case TypeDocument:
		if d := tm.GetDocument(id); d != nil {
			return d.Path
		}
	case TypeResource:
		if r := tm.GetResource(id); r != nil {
			return r.Path
		}
	}
	return ""
}

// DeleteItem deletes a file/folder with the given ID and type from disk.
// It returns false if no item with the given id and type exists.
func (tm *TreeManager) DeleteItem(id string, itemType string) (bool, error) {
	tm.fsLock.Lock()
	defer tm.fsLock.Unlock()

	if itemType == TypeSection && id == RootSectionID {
		return false, fmt.Errorf("%w: the root section cannot be deleted", ErrInvalidName)
	}
	path := tm.itemPath(id, itemType)
	if path == "" {
		return false, nil
	}

	if err := os.RemoveAll(path); err != nil {
		return false, err
	}
	tm.CreateItemTree()
	return true, nil
}
