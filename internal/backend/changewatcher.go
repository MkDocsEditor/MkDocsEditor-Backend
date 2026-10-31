package backend

import (
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// debounce delay: a burst of events (git pull, editor save) triggers one rebuild
const watcherDebounce = 200 * time.Millisecond

// FileWatcher watches the docs folder recursively and calls onChange (debounced) after changes
type FileWatcher struct {
	path     string
	onChange func()
	watcher  *fsnotify.Watcher
	done     chan struct{}
}

// NewFileWatcher starts watching path and all folders below it
func NewFileWatcher(path string, onChange func()) (*FileWatcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	fw := &FileWatcher{path: path, onChange: onChange, watcher: w, done: make(chan struct{})}
	fw.addRecursive(path)
	go fw.loop()
	return fw, nil
}

func (fw *FileWatcher) addRecursive(root string) {
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != root && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			if err := fw.watcher.Add(p); err != nil {
				log.Printf("Unable to watch %s: %v", p, err)
			}
		}
		return nil
	})
	if err != nil {
		log.Printf("Unable to watch %s: %v", root, err)
	}
}

func (fw *FileWatcher) loop() {
	var timer *time.Timer
	fire := make(chan struct{}, 1)
	for {
		select {
		case <-fw.done:
			if timer != nil {
				timer.Stop()
			}
			return
		case event, ok := <-fw.watcher.Events:
			if !ok {
				return
			}
			if event.Has(fsnotify.Create) {
				if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
					fw.addRecursive(event.Name)
				}
			}
			if timer == nil {
				timer = time.AfterFunc(watcherDebounce, func() {
					select {
					case fire <- struct{}{}:
					default:
					}
				})
			} else {
				timer.Reset(watcherDebounce)
			}
		case <-fire:
			fw.onChange()
		case err, ok := <-fw.watcher.Errors:
			if !ok {
				return
			}
			log.Printf("File watcher error: %v", err)
		}
	}
}

// Close stops watching
func (fw *FileWatcher) Close() {
	close(fw.done)
	_ = fw.watcher.Close()
}
