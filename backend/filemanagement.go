package backend

import (
	"os"
	"fmt"
	"io/ioutil"
)

// read the content of a file
func ReadFile(path string) (string, error) {
	data, err := ioutil.ReadFile(path)
	if isError(err) {
		return "", err
	}

	return string(data), nil
}

// create a new file with the given content
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

// update the content of an existing file
func UpdateFile(path string, content string) error {
	// open file using READ & WRITE permission
	var file, err = os.OpenFile(path, os.O_RDWR, 0644)
	if isError(err) {
		return err
	}
	defer file.Close()

	// write some text line-by-line to file
	_, err = file.WriteString(content)
	if isError(err) {
		return err
	}

	// save changes
	err = file.Sync()
	if isError(err) {
		return err
	}

	return nil
}

// delete a file from disk
func DeleteFile(path string) (bool, error) {
	var err = os.Remove(path)
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
