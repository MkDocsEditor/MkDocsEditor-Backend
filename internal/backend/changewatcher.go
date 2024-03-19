package backend

import (
	"fmt"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
	"github.com/fsnotify/fsnotify"
	"os"
	"path/filepath"
)

// file watcher
var watcher *fsnotify.Watcher

func InitFileWatcher(action func(s string)) {
	watchDirRecursive(configuration.CurrentConfig.MkDocs.DocsPath, action)
}

// watches all files and folders in the given path recursively
func watchDirRecursive(path string, action func(s string)) {
	// creates a new file watcher
	watcher, _ = fsnotify.NewWatcher()

	go func() {
		for {
			select {
			// watch for events
			case event := <-watcher.Events:
				//if (event.Op == fsnotify.Write) {
				fmt.Printf("EVENT! %#v\n", event)
				action(event.Name)
				//}

				// watch for errors
			case err := <-watcher.Errors:
				fmt.Println("ERROR", err)
			}
		}
	}()

	// starting at the root of the project, walk each file/directory searching for directories
	if err := filepath.Walk(path, addFolderWatch); err != nil {
		fmt.Println("ERROR", err)
	}
}

// adds a path to the watcher
func addFolderWatch(path string, fi os.FileInfo, err error) error {
	// since fsnotify can watch all the files in a directory, watchers only need
	// to be added to each nested directory
	if err != nil {
		return err
	}

	if fi.Mode().IsDir() {
		return watcher.Add(path)
	}

	return nil
}

// stop watching any files
func close() {
	watcher.Close()
}
