package backend

import (
	"github.com/patrickmn/go-cache"
	"time"
	"mkdocsrest/config"
	"path/filepath"
	"os"
	"fmt"
	"github.com/OneOfOne/xxhash"
	"errors"
	"strconv"
)

type Document struct {
	ID      string `json:"id" xml:"id" form:"id" query:"id"`
	Name    string `json:"name" xml:"name" form:"name" query:"name"`
	Content string `json:"content" xml:"content" form:"content" query:"content"`
}

var DataCache *cache.Cache

func SetupCache() {
	DataCache = cache.New(5*time.Minute, 10*time.Minute)
}

// traverses the mkdocs directory and puts all files into the cache
func UpdateCache() {
	searchDir := config.CurrentConfig.MkDocs.Path

	fileList := make([]string, 0)
	e := filepath.Walk(searchDir, func(path string, f os.FileInfo, err error) error {
		if f.IsDir() {
			// ignore directories
			return err
		}

		content, success := ReadFile(path)

		if !success {
			return errors.New(fmt.Sprintf("Unable to read file content of file '%s'", path))
		}

		var pathHash = xxhash.ChecksumString64(path)
		var pathHashAsString = strconv.FormatUint(pathHash, 10)

		var fileName = f.Name()

		d := &Document{
			ID:      pathHashAsString,
			Name:    fileName,
			Content: content,
		}

		DataCache.Add(pathHashAsString, d, cache.NoExpiration)

		fileList = append(fileList, path)
		return err
	})

	if e != nil {
		panic(e)
	}
}
