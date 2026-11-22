package api

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/jpeg"
	"strings"
	"testing"
)

// jpegSegment encodes an APPn segment
func jpegSegment(marker byte, payload []byte) []byte {
	seg := []byte{0xff, marker, 0, 0}
	binary.BigEndian.PutUint16(seg[2:], uint16(len(payload)+2))
	return append(seg, payload...)
}

// exifWithCamera returns the payload of an APP1 Exif segment with Make and Model
func exifWithCamera(maker, model string) []byte {
	tiff := []byte("II*\x00\x08\x00\x00\x00")
	tiff = binary.LittleEndian.AppendUint16(tiff, 2)
	values := []string{maker + "\x00", model + "\x00"}
	offset := 8 + 2 + 2*12 + 4
	var data []byte
	for i, tag := range []uint16{0x010f, 0x0110} {
		tiff = binary.LittleEndian.AppendUint16(tiff, tag)
		tiff = binary.LittleEndian.AppendUint16(tiff, 2)
		tiff = binary.LittleEndian.AppendUint32(tiff, uint32(len(values[i])))
		tiff = binary.LittleEndian.AppendUint32(tiff, uint32(offset+len(data)))
		data = append(data, values[i]...)
	}
	tiff = append(binary.LittleEndian.AppendUint32(tiff, 0), data...)
	return append([]byte("Exif\x00\x00"), tiff...)
}

// thermalJPEG is a JPEG like those of thermal cameras: Exif with the camera, raw data in APP3
// segments and more data after the end of the image
func thermalJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewGray(image.Rect(0, 0, w, h)), nil); err != nil {
		t.Fatal(err)
	}
	plain := buf.Bytes()
	out := append([]byte{}, plain[:2]...)
	out = append(out, jpegSegment(0xe1, exifWithCamera("InfiRay", "P2 Pro"))...)
	for range 3 {
		out = append(out, jpegSegment(0xe3, bytes.Repeat([]byte{0x42}, 60000))...)
	}
	out = append(out, plain[2:]...)
	return append(out, bytes.Repeat([]byte{7}, 1000)...)
}

func TestReadImageInfo(t *testing.T) {
	info := readImageInfo(thermalJPEG(t, 256, 192))
	if info.Width != 256 || info.Height != 192 || info.Camera != "InfiRay P2 Pro" || info.Trailing != 1000 {
		t.Fatalf("info = %+v", info)
	}
	if len(info.Segments) != 2 || info.Segments[1].Kind != "APP3" || info.Segments[1].Count != 3 || info.Segments[1].Bytes != 3*60004 {
		t.Fatalf("segments = %+v", info.Segments)
	}
	d := info.describe(128, 96)
	for _, want := range []string{"original: 256 x 192 px, shown scaled to 128 x 96 px", "camera: InfiRay P2 Pro",
		"APP1 Exif (", "APP3 x3 (175.8 KiB)", "after the end of the image: 1000 B", "download_resource"} {
		if !strings.Contains(d, want) {
			t.Errorf("describe lacks %q:\n%s", want, d)
		}
	}

	// an ordinary photo gets no note
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, image.NewGray(image.Rect(0, 0, 10, 10)), nil)
	plain := readImageInfo(buf.Bytes())
	if d = plain.describe(10, 10); d != "original: 10 x 10 px" {
		t.Errorf("plain JPEG = %q", d)
	}
}
