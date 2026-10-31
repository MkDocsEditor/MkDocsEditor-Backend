package backend

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	mutexSync "sync"
	"time"
)

const (
	stateDocumentsDir = "documents"
	stateDeletedDir   = "deleted"
	stateDocSuffix    = ".automerge"
	stateMetaSuffix   = ".json"
)

// DocumentMeta is the sidecar of a saved Automerge document
type DocumentMeta struct {
	// UUID is the stable internal id of the document state
	UUID string `json:"uuid"`
	// Path of the markdown file relative to the docs folder, slash separated
	Path string `json:"path"`
	// Epoch identifies the lineage of the Automerge document
	Epoch string `json:"epoch"`
	// DiskBaseHeads are the heads of the document whose text was last read from or written to disk
	DiskBaseHeads []string `json:"diskBaseHeads"`
	// PendingChanges counts the changes per username since the last git commit
	PendingChanges map[string]int `json:"pendingChanges,omitempty"`
	// LastChange is the time of the last change by a user
	LastChange time.Time `json:"lastChange,omitempty"`
}

// StateStore persists Automerge documents outside the docs folder.
//
//	<dir>/documents/<uuid>.automerge   Automerge save()
//	<dir>/documents/<uuid>.json        DocumentMeta
//	<dir>/documents/deleted/...        state of deleted documents
//
// State files are never deleted: corrupt files are renamed, deleted documents are moved.
type StateStore struct {
	dir string

	lock mutexSync.Mutex
	// active documents: path -> meta
	index map[string]*DocumentMeta
	// deleted documents: path -> meta (the most recently deleted one)
	deleted map[string]*DocumentMeta
}

// NewStateStore opens (and creates) the state directory and indexes the saved documents
func NewStateStore(dir string) (*StateStore, error) {
	s := &StateStore{
		dir:     dir,
		index:   map[string]*DocumentMeta{},
		deleted: map[string]*DocumentMeta{},
	}
	if err := os.MkdirAll(filepath.Join(dir, stateDocumentsDir, stateDeletedDir), 0o750); err != nil {
		return nil, err
	}
	if err := s.scan(filepath.Join(dir, stateDocumentsDir), s.index); err != nil {
		return nil, err
	}
	if err := s.scan(filepath.Join(dir, stateDocumentsDir, stateDeletedDir), s.deleted); err != nil {
		return nil, err
	}
	return s, nil
}

// Dir returns the state directory
func (s *StateStore) Dir() string {
	return s.dir
}

func (s *StateStore) scan(dir string, into map[string]*DocumentMeta) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), stateMetaSuffix) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return err
		}
		var meta DocumentMeta
		if err = json.Unmarshal(data, &meta); err != nil || meta.UUID == "" || meta.Path == "" {
			log.Printf("Ignoring invalid state sidecar %s: %v", e.Name(), err)
			continue
		}
		if existing, ok := into[meta.Path]; ok {
			// keep the newer one, the other stays on disk untouched
			if existing.LastChange.After(meta.LastChange) {
				continue
			}
		}
		m := meta
		into[meta.Path] = &m
	}
	return nil
}

func (s *StateStore) docFile(dir string, uuid string) string {
	return filepath.Join(dir, uuid+stateDocSuffix)
}

func (s *StateStore) metaFile(dir string, uuid string) string {
	return filepath.Join(dir, uuid+stateMetaSuffix)
}

func (s *StateStore) activeDir() string {
	return filepath.Join(s.dir, stateDocumentsDir)
}

func (s *StateStore) deletedDir() string {
	return filepath.Join(s.dir, stateDocumentsDir, stateDeletedDir)
}

// Load returns the meta and the saved document of the given path, or nil if there is none
func (s *StateStore) Load(relPath string) (*DocumentMeta, []byte, error) {
	s.lock.Lock()
	defer s.lock.Unlock()
	meta, ok := s.index[relPath]
	if !ok {
		return nil, nil, nil
	}
	data, err := os.ReadFile(s.docFile(s.activeDir(), meta.UUID))
	if err != nil {
		return copyMeta(meta), nil, err
	}
	return copyMeta(meta), data, nil
}

// RestoreDeleted moves the state of a deleted document with the given path back.
// It returns false if there is none.
func (s *StateStore) RestoreDeleted(relPath string) (bool, error) {
	s.lock.Lock()
	defer s.lock.Unlock()
	meta, ok := s.deleted[relPath]
	if !ok {
		return false, nil
	}
	if _, exists := s.index[relPath]; exists {
		return false, nil
	}
	for _, f := range []func(string, string) string{s.docFile, s.metaFile} {
		if err := os.Rename(f(s.deletedDir(), meta.UUID), f(s.activeDir(), meta.UUID)); err != nil {
			return false, err
		}
	}
	delete(s.deleted, relPath)
	s.index[relPath] = meta
	return true, nil
}

// Save writes the document and its meta
func (s *StateStore) Save(meta *DocumentMeta, doc []byte) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	if err := WriteFile(s.docFile(s.activeDir(), meta.UUID), doc); err != nil {
		return err
	}
	if err := s.writeMeta(s.activeDir(), meta); err != nil {
		return err
	}
	s.index[meta.Path] = copyMeta(meta)
	return nil
}

func (s *StateStore) writeMeta(dir string, meta *DocumentMeta) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return WriteFile(s.metaFile(dir, meta.UUID), data)
}

// Move updates the path of the document at oldPath, or of every document below it for a folder
func (s *StateStore) Move(oldPath string, newPath string) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	for p, meta := range s.index {
		moved, ok := movePath(p, oldPath, newPath)
		if !ok {
			continue
		}
		updated := copyMeta(meta)
		updated.Path = moved
		if err := s.writeMeta(s.activeDir(), updated); err != nil {
			return err
		}
		delete(s.index, p)
		s.index[moved] = updated
	}
	return nil
}

// Archive moves the state of the document at relPath, or of every document below it for a
// folder, into the deleted folder
func (s *StateStore) Archive(relPath string) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	for p, meta := range s.index {
		if _, ok := movePath(p, relPath, relPath); !ok {
			continue
		}
		for _, f := range []func(string, string) string{s.docFile, s.metaFile} {
			if err := os.Rename(f(s.activeDir(), meta.UUID), f(s.deletedDir(), meta.UUID)); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		delete(s.index, p)
		s.deleted[p] = meta
	}
	return nil
}

// MarkCorrupt renames the state of a document that cannot be loaded and forgets it
func (s *StateStore) MarkCorrupt(meta *DocumentMeta) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	suffix := ".corrupt-" + time.Now().UTC().Format("20060102T150405Z")
	for _, f := range []func(string, string) string{s.docFile, s.metaFile} {
		name := f(s.activeDir(), meta.UUID)
		if err := os.Rename(name, name+suffix); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	delete(s.index, meta.Path)
	return nil
}

// movePath maps p to its new path if p is oldPath or below it
func movePath(p string, oldPath string, newPath string) (string, bool) {
	if p == oldPath {
		return newPath, true
	}
	if strings.HasPrefix(p, oldPath+"/") {
		return path.Join(newPath, strings.TrimPrefix(p, oldPath+"/")), true
	}
	return "", false
}

func copyMeta(m *DocumentMeta) *DocumentMeta {
	c := *m
	c.DiskBaseHeads = append([]string(nil), m.DiskBaseHeads...)
	if m.PendingChanges != nil {
		c.PendingChanges = make(map[string]int, len(m.PendingChanges))
		for k, v := range m.PendingChanges {
			c.PendingChanges[k] = v
		}
	}
	return &c
}

// newRandomID returns 16 random bytes as hex
func newRandomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// checkStateDir makes sure the state directory is not inside the docs folder
func checkStateDir(stateDir string, docsDir string) error {
	rel, err := filepath.Rel(docsDir, stateDir)
	if err != nil {
		return err
	}
	if rel == "." || (!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)) {
		return fmt.Errorf("the state directory %s must not be inside the docs folder %s", stateDir, docsDir)
	}
	return nil
}

// ExcludeFromGit adds the state directory to .git/info/exclude of the repository in projectPath,
// if there is one and the state directory is inside it
func ExcludeFromGit(projectPath string, stateDir string) error {
	gitDir := filepath.Join(projectPath, ".git")
	if info, err := os.Stat(gitDir); err != nil || !info.IsDir() {
		return nil
	}
	rel, err := filepath.Rel(projectPath, stateDir)
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil
	}
	pattern := "/" + filepath.ToSlash(rel) + "/"
	excludeFile := filepath.Join(gitDir, "info", "exclude")
	existing, err := os.ReadFile(excludeFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, line := range strings.Split(string(existing), "\n") {
		if strings.TrimSpace(line) == pattern {
			return nil
		}
	}
	if err = os.MkdirAll(filepath.Dir(excludeFile), 0o750); err != nil {
		return err
	}
	content := string(existing)
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += "# MkDocsEditor backend state\n" + pattern + "\n"
	return WriteFile(excludeFile, []byte(content))
}
