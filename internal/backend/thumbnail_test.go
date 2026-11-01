package backend

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
)

// withJPEGOrientation inserts an EXIF APP1 segment with the orientation tag after the SOI marker
func withJPEGOrientation(t *testing.T, data []byte, orientation uint16) []byte {
	t.Helper()
	var tiff bytes.Buffer
	tiff.WriteString("MM")
	_ = binary.Write(&tiff, binary.BigEndian, uint16(42))
	_ = binary.Write(&tiff, binary.BigEndian, uint32(8))
	_ = binary.Write(&tiff, binary.BigEndian, uint16(1))
	_ = binary.Write(&tiff, binary.BigEndian, uint16(0x0112))
	_ = binary.Write(&tiff, binary.BigEndian, uint16(3))
	_ = binary.Write(&tiff, binary.BigEndian, uint32(1))
	_ = binary.Write(&tiff, binary.BigEndian, orientation)
	_ = binary.Write(&tiff, binary.BigEndian, uint16(0))
	_ = binary.Write(&tiff, binary.BigEndian, uint32(0))
	segment := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	var out bytes.Buffer
	out.Write(data[:2])
	out.Write([]byte{0xff, 0xe1})
	_ = binary.Write(&out, binary.BigEndian, uint16(len(segment)+2))
	out.Write(segment)
	out.Write(data[2:])
	return out.Bytes()
}

func withImages(t *testing.T) func(c *configuration.Configuration) {
	return func(c *configuration.Configuration) {
		docs := filepath.Join(c.MkDocs.ProjectPath, "docs", "img")
		if err := os.MkdirAll(docs, 0o755); err != nil {
			t.Fatal(err)
		}
		// 400x200, opaque, rotated by 90° clockwise through EXIF orientation 6
		photo := image.NewRGBA(image.Rect(0, 0, 400, 200))
		for y := range 200 {
			for x := range 400 {
				photo.Set(x, y, color.RGBA{R: uint8(x % 256), G: 100, B: 50, A: 255})
			}
		}
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, photo, nil); err != nil {
			t.Fatal(err)
		}
		write := func(name string, data []byte) {
			if err := os.WriteFile(filepath.Join(docs, name), data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		write("Bild vom Strand.jpg", withJPEGOrientation(t, buf.Bytes(), 6))
		// 100x50 with transparency
		icon := image.NewNRGBA(image.Rect(0, 0, 100, 50))
		icon.Set(1, 1, color.NRGBA{R: 255, A: 128})
		buf.Reset()
		if err := png.Encode(&buf, icon); err != nil {
			t.Fatal(err)
		}
		write("icon.png", buf.Bytes())
		write("logo.svg", []byte("<svg xmlns=\"http://www.w3.org/2000/svg\"/>"))
		write("kaputt.png", []byte("\x89PNG not really"))
	}
}

func TestResourceThumbnail(t *testing.T) {
	e := newRestTestEnv(t, withImages(t))
	tree := e.tm.Root()
	photo := findResourceByName(tree, "Bild vom Strand.jpg")
	if !photo.Thumbnail || findResourceByName(tree, "logo.svg").Thumbnail {
		t.Error("thumbnail flags")
	}

	rec := e.do(http.MethodGet, "/resource/"+photo.ID+"/thumbnail/?size=100", nil, "")
	expectStatus(t, rec, http.StatusOK)
	if rec.Header().Get("Content-Type") != "image/jpeg" || rec.Header().Get("ETag") == "" {
		t.Errorf("headers: %v", rec.Header())
	}
	img, err := jpeg.Decode(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	// size 100 is rounded up to 128; 400x200 becomes 128x64, then turned upright: 64x128
	if b := img.Bounds(); b.Dx() != 64 || b.Dy() != 128 {
		t.Errorf("thumbnail is %dx%d", b.Dx(), b.Dy())
	}
	cached, _ := filepath.Glob(filepath.Join(e.project, ".mkdocseditor", thumbnailsDir, "*.jpg"))
	if len(cached) != 1 {
		t.Errorf("cache: %v", cached)
	}
	// the cached thumbnail and the ETag
	again := e.do(http.MethodGet, "/resource/"+photo.ID+"/thumbnail/?size=128", nil, "")
	if !bytes.Equal(again.Body.Bytes(), rec.Body.Bytes()) {
		t.Error("the cached thumbnail differs")
	}
	req, _ := http.NewRequest(http.MethodGet, "/resource/"+photo.ID+"/thumbnail/?size=128", nil)
	req.Header.Set("If-None-Match", rec.Header().Get("ETag"))
	rec304 := e.doRequest(req)
	expectStatus(t, rec304, http.StatusNotModified)

	// transparency stays: PNG; small images are not upscaled
	rec = e.do(http.MethodGet, "/resource/"+findResourceByName(tree, "icon.png").ID+"/thumbnail/", nil, "")
	expectStatus(t, rec, http.StatusOK)
	icon, err := png.Decode(bytes.NewReader(rec.Body.Bytes()))
	if err != nil || rec.Header().Get("Content-Type") != "image/png" || icon.Bounds().Dx() != 100 {
		t.Errorf("icon: %v %v", err, rec.Header())
	}

	expectStatus(t, e.do(http.MethodGet, "/resource/"+findResourceByName(tree, "logo.svg").ID+"/thumbnail/", nil, ""), http.StatusNotFound)
	expectStatus(t, e.do(http.MethodGet, "/resource/"+findResourceByName(tree, "kaputt.png").ID+"/thumbnail/", nil, ""), http.StatusNotFound)
	expectStatus(t, e.do(http.MethodGet, "/resource/"+photo.ID+"/thumbnail/?size=5000", nil, ""), http.StatusBadRequest)
	expectStatus(t, e.do(http.MethodGet, "/resource/nope/thumbnail/", nil, ""), http.StatusNotFound)
}

func TestExifOrientations(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 3, 2))
	src.SetNRGBA(0, 0, color.NRGBA{R: 255, A: 255})
	// where the top left pixel ends up for each orientation
	want := map[int]image.Point{1: {0, 0}, 2: {2, 0}, 3: {2, 1}, 4: {0, 1}, 5: {0, 0}, 6: {1, 0}, 7: {1, 2}, 8: {0, 2}}
	for o, p := range want {
		out := orient(src, o)
		if r, _, _, _ := out.At(p.X, p.Y).RGBA(); r == 0 {
			t.Errorf("orientation %d: the red pixel is not at %v", o, p)
		}
	}
}
