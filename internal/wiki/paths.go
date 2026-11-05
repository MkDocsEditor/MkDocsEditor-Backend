package wiki

import (
	"path"
	"strings"
)

// IsInside reports whether p is the path dir or below it ("" is the docs folder)
func IsInside(p string, dir string) bool {
	if dir == "" {
		return true
	}
	_, ok := MovePath(p, dir, dir)
	return ok
}

// MovePath maps p to its new path if p is oldPath or below it
func MovePath(p string, oldPath string, newPath string) (string, bool) {
	if p == oldPath {
		return newPath, true
	}
	if after, ok := strings.CutPrefix(p, oldPath+"/"); ok {
		return path.Join(newPath, after), true
	}
	return "", false
}
