package backend

import (
	"fmt"
	"io"
	"io/ioutil"
	"mime/multipart"
	"os"
)

// ReadFile read the content of a file
func ReadFile(path string) (string, error) {
	data, err := ioutil.ReadFile(path)
	if isError(err) {
		return "", err
	}

	return string(data), nil
}

func WriteFile(path string, data []byte) (err error) {
	err = ioutil.WriteFile(path, data, 0x660)
	isError(err)
	return err
}

// CreateFile create a new file with the given content
func CreateFile(path string, content string) {
	// detect if file exists
	var _, err = os.Stat(path)

	// create file if not exists
	if os.IsNotExist(err) {
		var file, err = os.Create(path)
		if isError(err) {
			return
		}
		defer file.Close()
	}
}

// UpdateFileFromForm update the content of an existing file
func UpdateFileFromForm(file *multipart.FileHeader, targetPath string) error {
	src, err := file.Open()
	if err != nil {
		return err
	}
	defer src.Close()

	// Destination
	dst, err := os.Create(targetPath)
	if err != nil {
		return err
	}
	defer dst.Close()

	// Copy
	if _, err = io.Copy(dst, src); err != nil {
		return err
	}

	return nil
}

// DeleteFileOrFolder delete a file from disk
func DeleteFileOrFolder(path string) (bool, error) {
	var err = os.RemoveAll(path)
	if isError(err) {
		return false, err
	}
	return true, nil
}

// check if an error exists
func isError(err error) bool {
	if err != nil {
		fmt.Println(err.Error())
	}

	return err != nil
}
