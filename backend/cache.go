package backend

import (
	"github.com/patrickmn/go-cache"
	"time"
	"mkdocsrest/config"
	"path/filepath"
	"os"
)

var DataCache *cache.Cache

func SetupCache() {
	DataCache = cache.New(5*time.Minute, 10*time.Minute)
}

// traverses the mkdocs directory and puts all files into the cache
func UpdateCache() {
	searchDir := config.CurrentConfig.MkDocs.Path

	e := filepath.Walk(searchDir, func(path string, f os.FileInfo, err error) error {
		return err
	})

	if e != nil {
		panic(e)
	}
}
