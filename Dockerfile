# syntax=docker/dockerfile:1

# Build stage. automerge-go links a prebuilt Rust static library through cgo,
# so the binary is built with cgo and linked statically against glibc.
FROM golang:1.27.3-trixie AS build

WORKDIR /src

# The same for both builds below, or the compiled dependencies are not reused.
ENV CGO_ENABLED=1
ENV GOFLAGS="-trimpath -tags=netgo,osusergo"

COPY go.mod go.sum ./
RUN go mod download

# Compile the dependencies in a layer of their own. The final build runs for every commit because
# the version arguments change, but this layer only changes with go.mod and go.sum. These are the
# packages the backend imports (go list -f '{{join .Imports "\n"}}' ./...). One that is missing
# only makes the final build slower. One that is no longer in go.mod fails here: remove it.
RUN go build \
    github.com/OneOfOne/xxhash \
    github.com/automerge/automerge-go \
    github.com/fatih/color \
    github.com/fsnotify/fsnotify \
    github.com/go-webauthn/webauthn/protocol \
    github.com/go-webauthn/webauthn/protocol/webauthncbor \
    github.com/go-webauthn/webauthn/protocol/webauthncose \
    github.com/go-webauthn/webauthn/webauthn \
    github.com/google/uuid \
    github.com/gorilla/websocket \
    github.com/labstack/echo/v5 \
    github.com/labstack/echo/v5/middleware \
    github.com/prometheus/client_golang/prometheus \
    github.com/prometheus/client_golang/prometheus/collectors \
    github.com/prometheus/client_golang/prometheus/promauto \
    github.com/prometheus/client_golang/prometheus/promhttp \
    github.com/pterm/pterm \
    github.com/sergi/go-diff/diffmatchpatch \
    github.com/spf13/cobra \
    github.com/spf13/viper \
    golang.org/x/crypto/argon2 \
    golang.org/x/crypto/bcrypt \
    golang.org/x/image/draw \
    golang.org/x/image/webp \
    golang.org/x/term \
    golang.org/x/text/unicode/norm \
    gopkg.in/yaml.v3 \
    modernc.org/sqlite

COPY . .

ARG VERSION=dev
ARG GIT_REV=unknown
ARG BUILD_DATE=unknown

RUN go build \
    -ldflags "-w -s -linkmode external -extldflags '-static' \
      -X github.com/MkDocsEditor/MkDocsEditor-Backend/cmd/global.Version=${VERSION} \
      -X github.com/MkDocsEditor/MkDocsEditor-Backend/cmd/global.Commit=${GIT_REV} \
      -X github.com/MkDocsEditor/MkDocsEditor-Backend/cmd/global.Date=${BUILD_DATE}" \
    -o /out/mkdocsrest . \
 && /out/mkdocsrest --version

# Runtime stage
FROM alpine:3.22

RUN apk add --no-cache ca-certificates git tzdata \
 && addgroup -g 1000 -S mkdocs \
 && adduser -u 1000 -S -G mkdocs -h /app mkdocs

WORKDIR /app
COPY --link --from=build /out/mkdocsrest /app/main

USER 1000:1000
# 7413 is the API, 9413 the metrics (only with server.metrics.enabled)
EXPOSE 7413 9413

# The configuration is read from /app/mkdocsrest.yaml (mount it there).
CMD ["/app/main"]
