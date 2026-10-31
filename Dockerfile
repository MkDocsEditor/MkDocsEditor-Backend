# syntax=docker/dockerfile:1

# Build stage. automerge-go links a prebuilt Rust static library through cgo,
# so the binary is built with cgo and linked statically against glibc.
FROM golang:1.27.3-trixie AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG GIT_REV=unknown
ARG BUILD_DATE=unknown

RUN CGO_ENABLED=1 go build \
    -trimpath \
    -tags netgo,osusergo \
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
COPY --from=build /out/mkdocsrest /app/main

USER 1000:1000
EXPOSE 7413

# The configuration is read from /app/mkdocsrest.yaml (mount it there).
CMD ["/app/main"]
