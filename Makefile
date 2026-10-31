GO_FLAGS   ?=
NAME       := mkdocseditor-backend
OUTPUT_BIN ?= bin/${NAME}
PACKAGE    := github.com/MkDocsEditor/MkDocsEditor-Backend
GIT_REV    ?= $(shell git rev-parse --short HEAD)
SOURCE_DATE_EPOCH ?= $(shell date +%s)
DATE       ?= $(shell date -u -d @${SOURCE_DATE_EPOCH} +"%Y-%m-%dT%H:%M:%SZ")
VERSION    ?= 0.7.0
LDFLAGS    := -w -s -X ${PACKAGE}/cmd/global.Version=${VERSION} -X ${PACKAGE}/cmd/global.Commit=${GIT_REV} -X ${PACKAGE}/cmd/global.Date=${DATE}

.PHONY: test lint build run docker clean

test:   ## Run all tests (with the race detector)
	@go test -race -count=1 ./...

lint:   ## Run golangci-lint
	@golangci-lint run ./...

build:  ## Builds the binary (automerge-go needs cgo)
	@CGO_ENABLED=1 go build ${GO_FLAGS} -trimpath -ldflags "${LDFLAGS}" -o ${OUTPUT_BIN} .

run: build
	./${OUTPUT_BIN}

docker: ## Builds the Docker image
	docker build --build-arg VERSION=${VERSION} --build-arg GIT_REV=${GIT_REV} --build-arg BUILD_DATE=${DATE} -t ${NAME}:${VERSION} .

clean:
	go clean
	rm -f ${OUTPUT_BIN}
