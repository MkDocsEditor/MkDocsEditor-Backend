package backend

import (
	"mkdocsrest/config"
	"path/filepath"
	"os"
	"github.com/OneOfOne/xxhash"
	"strconv"
	"io/ioutil"
	"log"
	"strings"
)

type Section struct {
	Name        string      `json:"name" xml:"name" form:"name" query:"name"`
	Subsections *[]Section  `json:"subsections" xml:"subsections" form:"subsections" query:"subsections"`
	Documents   *[]Document `json:"documents" xml:"documents" form:"documents" query:"documents"`
}

type Document struct {
	ID       string `json:"id" xml:"id" form:"id" query:"id"`
	Name     string `json:"name" xml:"name" form:"name" query:"name"`
	Path     string `json:"path" xml:"path" form:"path" query:"path"`
	Filesize int64  `json:"filesize" xml:"filesize" form:"filesize" query:"filesize"`
	Content  string `json:"content" xml:"content" form:"content" query:"content"`
}

type DocumentDescription struct {
	ID       string `json:"id" xml:"id" form:"id" query:"id"`
	Name     string `json:"name" xml:"name" form:"name" query:"name"`
	Path     string `json:"path" xml:"path" form:"path" query:"path"`
	Filesize int64  `json:"filesize" xml:"filesize" form:"filesize" query:"filesize"`
}

// an in memory representation of the mkdocs file structure
var DocumentTree = &Section{
	Name:        "root",
	Documents:   &[]Document{},
	Subsections: &[]Section{},
}

// traverses the mkdocs directory and puts all files into the cache
func CreateDocumentTree() {
	searchDir := config.CurrentConfig.MkDocs.Path
	populateDocumentTree(DocumentTree, filepath.Join(searchDir, "docs"))
}

func populateDocumentTree(section *Section, path string) {
	files, err := ioutil.ReadDir(path)
	if err != nil {
		log.Fatal(err)
	}

	for _, f := range files {
		if f.IsDir() {
			var subSection = createSection(path, f)
			*section.Subsections = append(*section.Subsections, subSection)

			// recursive call
			populateDocumentTree(&subSection, filepath.Join(path, subSection.Name))
		} else {
			if strings.HasSuffix(f.Name(), ".md") {
				var document = createDocument(path, f)
				*section.Documents = append(*section.Documents, document)
			}
		}
	}
}

func createSection(path string, info os.FileInfo) Section {
	return Section{
		Name:        info.Name(),
		Documents:   &[]Document{},
		Subsections: &[]Section{},
	}
}

func createDocument(path string, f os.FileInfo) Document {
	_, err := ReadFile(filepath.Join(path, f.Name()))

	if isError(err) {
		panic(err)
	}

	var pathHash = xxhash.ChecksumString64(path)
	var pathHashAsString = strconv.FormatUint(pathHash, 10)

	var fileName = f.Name()
	var fileSize = f.Size()
	var relativeFilePath = path

	return Document{
		ID:       pathHashAsString,
		Name:     fileName,
		Path:     relativeFilePath,
		Filesize: fileSize,
		Content:  "",
	}
}
