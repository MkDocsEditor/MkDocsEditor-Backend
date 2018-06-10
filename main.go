package main

import (
	"mkdocsrest/backend"
	"mkdocsrest/frontend"
)

// main entry point
func main() {
	backend.CreateItemTree()
	frontend.SetupRestService()
}
