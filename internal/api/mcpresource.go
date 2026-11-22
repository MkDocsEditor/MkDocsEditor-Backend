package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/references"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The MCP tools read_resource and download_resource: the files of the wiki for agents, which
// cannot use GET /resource/{id}/content/ (OAuth tokens only work for the MCP endpoint).
// See docs/mcp.md.

const (
	// mcpImageDefaultSize is the default longer side of an image; vision models scale larger
	// images down anyway
	mcpImageDefaultSize = 1024
	// mcpMaxTextBytes is the largest text file read_resource returns
	mcpMaxTextBytes = 256 << 10
	// mcpMaxDownloadBytes is the largest file download_resource returns; base64 makes the
	// answer a third larger
	mcpMaxDownloadBytes = 64 << 20
)

type (
	mcpResourceInput struct {
		Resource string `json:"resource" jsonschema:"file path relative to the wiki root (like guide/img/photo.jpg), a link destination from a page (with document), or the resource id"`
		Document string `json:"document,omitempty" jsonschema:"page the link is in: relative links are resolved from it"`
		Size     int    `json:"size,omitempty" jsonschema:"images: longer side in pixels, 64 to 1024, default 1024"`
	}

	mcpResourceRef struct {
		Resource string `json:"resource" jsonschema:"file path relative to the wiki root (like guide/img/photo.jpg), a link destination from a page (with document), or the resource id"`
		Document string `json:"document,omitempty" jsonschema:"page the link is in: relative links are resolved from it"`
	}
)

// resource finds a resource by id, by path relative to the wiki root, or by a link destination
// of a page
func (t *mcpTools) resource(ref, document string) (*wiki.Resource, error) {
	ref = strings.TrimSpace(ref)
	tm := t.rs.treeManager
	if r := tm.GetResource(ref); r != nil {
		return r, nil
	}
	from := ""
	if strings.TrimSpace(document) != "" {
		d, err := t.document(document)
		if err != nil {
			return nil, err
		}
		from = d.RelPath
	}
	dest, ok := references.ParseDestination(from, ref)
	if ok && !dest.Outside && dest.Path != "" {
		if r := tm.GetResource(tm.IDOfRelPath(dest.Path)); r != nil {
			return r, nil
		}
	}
	if from == "" {
		return nil, fmt.Errorf("no file %q: give the path relative to the wiki root, or the page of a relative link as document", ref)
	}
	return nil, fmt.Errorf("no file %q linked from %s", ref, from)
}

func (t *mcpTools) readResource(ctx context.Context, _ *mcp.CallToolRequest, in mcpResourceInput) (*mcp.CallToolResult, any, error) {
	r, err := t.resource(in.Resource, in.Document)
	if err != nil {
		return nil, nil, err
	}
	header := fmt.Sprintf("path: %s\nmimeType: %s\nsize: %d bytes", r.RelPath, r.MimeType, r.Filesize)

	if wiki.HasThumbnail(r.MimeType) {
		size, err := mcpImageSize(in.Size)
		if err != nil {
			return nil, nil, err
		}
		data, ext, err := t.rs.thumbnail(ctx, r, size)
		if errors.Is(err, errNoThumbnail) {
			return nil, nil, fmt.Errorf("%s: the image cannot be decoded or is too large", r.RelPath)
		}
		if err != nil {
			return nil, nil, err
		}
		if details := t.imageDetails(r, data); details != "" {
			header += "\n" + details
		}
		return &mcp.CallToolResult{Content: []mcp.Content{
			&mcp.TextContent{Text: header},
			&mcp.ImageContent{Data: data, MIMEType: thumbnailContentType(ext)},
		}}, nil, nil
	}

	if wiki.ResourceKind(r.MimeType) == wiki.ResourceKindText || r.MimeType == "image/svg+xml" {
		if r.Filesize > mcpMaxTextBytes {
			return nil, nil, fmt.Errorf("%s has %d bytes, read_resource returns text files up to %d bytes", r.RelPath, r.Filesize, mcpMaxTextBytes)
		}
		data, err := os.ReadFile(r.Path)
		if err != nil {
			return nil, nil, err
		}
		if !utf8.Valid(data) {
			return nil, nil, fmt.Errorf("%s is not UTF-8 text", r.RelPath)
		}
		return text(header + "\n---\n" + string(data)), nil, nil
	}

	return nil, nil, fmt.Errorf("%s (%s) is neither an image (PNG, JPEG, GIF, WebP) nor a text file: read_resource cannot "+
		"show it, download_resource gets the file", r.RelPath, r.MimeType)
}

// imageDetails describes the original of an image next to its scaled copy: dimensions, camera,
// metadata segments, and a note when the file holds data the copy lacks
func (t *mcpTools) imageDetails(r *wiki.Resource, scaled []byte) string {
	data, err := os.ReadFile(r.Path)
	if err != nil {
		return ""
	}
	info := readImageInfo(data)
	if bytes.HasPrefix(data, []byte("\xff\xd8")) && jpegOrientation(data) >= 5 {
		// the scaled copy is turned upright
		info.Width, info.Height = info.Height, info.Width
	}
	w, h := info.Width, info.Height
	if config, _, err := decodeConfig(scaled); err == nil {
		w, h = config.Width, config.Height
	}
	return info.describe(w, h)
}

// downloadResource returns a file unchanged as an embedded resource. Claude Code saves such
// blobs to a file and gives the agent its path, but shows image blobs as images: images are
// sent as application/octet-stream to keep them a file.
func (t *mcpTools) downloadResource(_ context.Context, _ *mcp.CallToolRequest, in mcpResourceRef) (*mcp.CallToolResult, any, error) {
	r, err := t.resource(in.Resource, in.Document)
	if err != nil {
		return nil, nil, err
	}
	if r.Filesize > mcpMaxDownloadBytes {
		return nil, nil, fmt.Errorf("%s has %s, download_resource returns files up to %s", r.RelPath, formatBytes(int(r.Filesize)), formatBytes(mcpMaxDownloadBytes))
	}
	data, err := os.ReadFile(r.Path)
	if err != nil {
		return nil, nil, err
	}
	mimeType := r.MimeType
	if mimeType == "" || strings.HasPrefix(mimeType, "image/") {
		mimeType = "application/octet-stream"
	}
	return &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.TextContent{Text: fmt.Sprintf("path: %s\nmimeType: %s\nsize: %d bytes\nThe file follows unchanged as an embedded "+
			"resource (sent as %s); your client may save it to a file and give you its path.", r.RelPath, r.MimeType, len(data), mimeType)},
		&mcp.EmbeddedResource{Resource: &mcp.ResourceContents{URI: mcpResourceURI(r), MIMEType: mimeType, Blob: data}},
	}}, nil, nil
}

// mcpResourceURI names a file of the wiki in embedded resources
func mcpResourceURI(r *wiki.Resource) string {
	return (&url.URL{Scheme: "wiki", Path: "/" + r.RelPath}).String()
}

// mcpImageSize rounds a requested image size up to a thumbnail size, 0 is the default
func mcpImageSize(n int) (int, error) {
	if n == 0 {
		return mcpImageDefaultSize, nil
	}
	return thumbnailSize(strconv.Itoa(n))
}
