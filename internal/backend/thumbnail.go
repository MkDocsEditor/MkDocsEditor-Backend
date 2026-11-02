package backend

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/OneOfOne/xxhash"
	"github.com/labstack/echo/v4"
	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/webp"
)

const (
	thumbnailsDir        = "thumbnails"
	thumbnailMaxPixels   = 50_000_000
	thumbnailDefaultSize = 256
	// image decoding is expensive, so only this many run at the same time
	maxConcurrentThumbnails = 2
)

// thumbnail sizes; a requested size is rounded up to the next one
var thumbnailSizes = []int{64, 128, 256, 512, 1024}

// errNoThumbnail is returned for files that get no thumbnail
var errNoThumbnail = errors.New("no thumbnail for this file")

// thumbnailMimeTypes are the image types that get thumbnails
var thumbnailMimeTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// hasThumbnail reports whether GET /resource/{id}/thumbnail/ works for a MIME type
func hasThumbnail(mimeType string) bool {
	return thumbnailMimeTypes[mimeType]
}

// thumbnailSize rounds a requested size up to one of the thumbnail sizes
func thumbnailSize(param string) (int, error) {
	if param == "" {
		return thumbnailDefaultSize, nil
	}
	n, err := strconv.Atoi(param)
	if err != nil || n < thumbnailSizes[0] || n > thumbnailSizes[len(thumbnailSizes)-1] {
		return 0, fmt.Errorf("size must be between %d and %d", thumbnailSizes[0], thumbnailSizes[len(thumbnailSizes)-1])
	}
	for _, s := range thumbnailSizes {
		if n <= s {
			return s, nil
		}
	}
	return thumbnailSizes[len(thumbnailSizes)-1], nil
}

// GET /resource/:id/thumbnail/?size=
func (rs *RestService) getResourceThumbnail(c echo.Context) error {
	id := c.Param(urlParamID)
	r := rs.treeManager.GetResource(id)
	if r == nil {
		return rs.ReturnNotFound(c, id)
	}
	size, err := thumbnailSize(c.QueryParam("size"))
	if err != nil {
		return apiError(c, http.StatusBadRequest, CodeBadRequest, err.Error(), map[string]any{"field": "size"})
	}
	if !hasThumbnail(r.MimeType) {
		return apiError(c, http.StatusNotFound, CodeNotFound, errNoThumbnail.Error(), nil)
	}
	key := thumbnailKey(r, size)
	etag := `"` + key + `"`
	c.Response().Header().Set("Cache-Control", "private, max-age=86400")
	c.Response().Header().Set("ETag", etag)
	if match := c.Request().Header.Get("If-None-Match"); match == etag {
		return c.NoContent(http.StatusNotModified)
	}

	dir := rs.thumbnailDir()
	for _, ext := range []string{".jpg", ".png"} {
		if data, err := os.ReadFile(filepath.Join(dir, key+ext)); err == nil {
			return c.Blob(http.StatusOK, thumbnailContentType(ext), data)
		}
	}
	data, ext, err := rs.renderThumbnail(c.Request().Context(), r, size)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		// the client is gone, nobody reads this
		return c.NoContent(http.StatusServiceUnavailable)
	}
	if errors.Is(err, errNoThumbnail) {
		return apiError(c, http.StatusNotFound, CodeNotFound, err.Error(), nil)
	}
	if err != nil {
		return rs.ReturnError(c, err)
	}
	if err = os.MkdirAll(dir, 0o750); err == nil {
		err = WriteFile(filepath.Join(dir, key+ext), data)
	}
	if err != nil {
		// serving it matters more than caching it
		c.Logger().Warnf("Unable to cache the thumbnail of %s: %v", r.RelPath, err)
	}
	return c.Blob(http.StatusOK, thumbnailContentType(ext), data)
}

// renderThumbnail makes a thumbnail in one of the limited slots. It gives up waiting for a slot
// when ctx ends.
func (rs *RestService) renderThumbnail(ctx context.Context, r *Resource, size int) ([]byte, string, error) {
	select {
	case rs.thumbnailSlots <- struct{}{}:
	case <-ctx.Done():
		return nil, "", ctx.Err()
	}
	defer func() { <-rs.thumbnailSlots }()
	return makeThumbnail(r.Path, size)
}

// thumbnailKey is the name of a cached thumbnail, without the extension. It changes with the
// path and the content of the resource.
func thumbnailKey(r *Resource, size int) string {
	return strconv.FormatUint(xxhash.ChecksumString64(fmt.Sprintf("%s|%d|%d|%d", r.RelPath, size, r.Filesize, r.ModTime.UnixNano())), 16)
}

func (rs *RestService) thumbnailDir() string {
	return filepath.Join(rs.sync.store.Dir(), thumbnailsDir)
}

// pruneThumbnails removes the cached thumbnails that belong to no current version of a resource
// in the tree: the resource was deleted, moved or changed. Hidden files are temporary files of
// thumbnails that are being written.
func (rs *RestService) pruneThumbnails(root *Section) {
	entries, err := os.ReadDir(rs.thumbnailDir())
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Printf("Unable to clean up the thumbnails: %v", err)
		}
		return
	}
	keep := map[string]bool{}
	for s := range root.walk() {
		for _, r := range s.Resources {
			if hasThumbnail(r.MimeType) {
				for _, size := range thumbnailSizes {
					keep[thumbnailKey(r, size)] = true
				}
			}
		}
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".") || keep[strings.TrimSuffix(name, filepath.Ext(name))] {
			continue
		}
		if err = os.Remove(filepath.Join(rs.thumbnailDir(), name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.Printf("Unable to remove the thumbnail %s: %v", name, err)
		}
	}
}

func thumbnailContentType(ext string) string {
	if ext == ".png" {
		return "image/png"
	}
	return "image/jpeg"
}

// makeThumbnail scales an image so that its longer side is at most size pixels. Images with
// transparency become PNG, others JPEG. JPEG orientation (EXIF) is applied.
func makeThumbnail(path string, size int) ([]byte, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	config, format, err := decodeConfig(data)
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return nil, "", errNoThumbnail
	}
	if int64(config.Width)*int64(config.Height) > thumbnailMaxPixels {
		return nil, "", errNoThumbnail
	}
	src, err := decodeImage(data, format)
	if err != nil {
		return nil, "", errNoThumbnail
	}

	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	if w > size || h > size {
		if w >= h {
			w, h = size, max(1, h*size/w)
		} else {
			w, h = max(1, w*size/h), size
		}
	}
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Src, nil)
	var out image.Image = dst
	if format == "jpeg" {
		out = orient(dst, jpegOrientation(data))
	}

	var buf bytes.Buffer
	if !dst.Opaque() {
		err = png.Encode(&buf, out)
		return buf.Bytes(), ".png", err
	}
	err = jpeg.Encode(&buf, out, &jpeg.Options{Quality: 80})
	return buf.Bytes(), ".jpg", err
}

func decodeConfig(data []byte) (image.Config, string, error) {
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG")):
		c, err := png.DecodeConfig(bytes.NewReader(data))
		return c, "png", err
	case bytes.HasPrefix(data, []byte("\xff\xd8")):
		c, err := jpeg.DecodeConfig(bytes.NewReader(data))
		return c, "jpeg", err
	case bytes.HasPrefix(data, []byte("GIF8")):
		c, err := gif.DecodeConfig(bytes.NewReader(data))
		return c, "gif", err
	case len(data) > 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		c, err := webp.DecodeConfig(bytes.NewReader(data))
		return c, "webp", err
	}
	return image.Config{}, "", errNoThumbnail
}

func decodeImage(data []byte, format string) (image.Image, error) {
	r := bytes.NewReader(data)
	switch format {
	case "png":
		return png.Decode(r)
	case "jpeg":
		return jpeg.Decode(r)
	case "gif":
		return gif.Decode(r)
	case "webp":
		return webp.Decode(r)
	}
	return nil, errNoThumbnail
}

// jpegOrientation returns the EXIF orientation (1 to 8) of a JPEG file, 1 if there is none
func jpegOrientation(data []byte) int {
	r := bytes.NewReader(data[2:])
	for {
		var marker [4]byte
		if _, err := io.ReadFull(r, marker[:]); err != nil || marker[0] != 0xff {
			return 1
		}
		length := int(binary.BigEndian.Uint16(marker[2:])) - 2
		if length < 0 || marker[1] == 0xda {
			return 1
		}
		segment := make([]byte, length)
		if _, err := io.ReadFull(r, segment); err != nil {
			return 1
		}
		if marker[1] == 0xe1 && strings.HasPrefix(string(segment), "Exif\x00\x00") {
			return exifOrientation(segment[6:])
		}
	}
}

// exifOrientation reads tag 0x0112 of IFD0 of a TIFF structure
func exifOrientation(tiff []byte) int {
	if len(tiff) < 8 {
		return 1
	}
	var order binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 1
	}
	ifd := int(order.Uint32(tiff[4:8]))
	if ifd+2 > len(tiff) {
		return 1
	}
	entries := int(order.Uint16(tiff[ifd:]))
	for i := range entries {
		e := ifd + 2 + 12*i
		if e+12 > len(tiff) {
			return 1
		}
		if order.Uint16(tiff[e:]) == 0x0112 {
			if v := int(order.Uint16(tiff[e+8:])); v >= 1 && v <= 8 {
				return v
			}
			return 1
		}
	}
	return 1
}

// orient turns an image as EXIF orientation o says
func orient(src *image.NRGBA, o int) image.Image {
	if o <= 1 || o > 8 {
		return src
	}
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	dw, dh := w, h
	if o >= 5 {
		dw, dh = h, w
	}
	dst := image.NewNRGBA(image.Rect(0, 0, dw, dh))
	for y := range h {
		for x := range w {
			var dx, dy int
			switch o {
			case 2:
				dx, dy = w-1-x, y
			case 3:
				dx, dy = w-1-x, h-1-y
			case 4:
				dx, dy = x, h-1-y
			case 5:
				dx, dy = y, x
			case 6:
				dx, dy = h-1-y, x
			case 7:
				dx, dy = h-1-y, w-1-x
			case 8:
				dx, dy = y, w-1-x
			}
			dst.SetNRGBA(dx, dy, src.NRGBAAt(x, y))
		}
	}
	return dst
}
