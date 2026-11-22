package api

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
)

// What an image file holds besides the picture: the camera, metadata segments and data after
// the end of the image. read_resource shows it, so an agent knows when the original file
// (download_resource) has more than a scaled copy, like the raw data of a thermal camera.

// imageExtraThreshold is the size of extra data from which read_resource points to the
// original: smaller segments are ordinary metadata
const imageExtraThreshold = 64 << 10

type (
	imageInfo struct {
		Width, Height int
		Camera        string
		// Segments are the metadata segments (JPEG APPn, PNG ancillary chunks), in file order
		// and merged by kind
		Segments []imageSegment
		// Trailing is the number of bytes after the end of the image
		Trailing int
	}

	imageSegment struct {
		// Kind is the marker and identifier, like "APP1 Exif" or "PNG chunk tEXt"
		Kind  string
		Count int
		Bytes int
		// Standard is true for segments of ordinary metadata (JFIF, Exif, ICC, XMP)
		Standard bool
	}
)

// readImageInfo examines a PNG or JPEG file. Other formats only get their dimensions.
func readImageInfo(data []byte) imageInfo {
	var info imageInfo
	if config, _, err := decodeConfig(data); err == nil {
		info.Width, info.Height = config.Width, config.Height
	}
	switch {
	case bytes.HasPrefix(data, []byte("\xff\xd8")):
		info.readJPEG(data)
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		info.readPNG(data)
	}
	return info
}

func (info *imageInfo) addSegment(kind string, size int, standard bool) {
	if n := len(info.Segments); n > 0 && info.Segments[n-1].Kind == kind {
		info.Segments[n-1].Count++
		info.Segments[n-1].Bytes += size
		return
	}
	info.Segments = append(info.Segments, imageSegment{Kind: kind, Count: 1, Bytes: size, Standard: standard})
}

// extraBytes is the size of the data that is neither the picture nor ordinary metadata
func (info *imageInfo) extraBytes() int {
	n := info.Trailing
	for _, s := range info.Segments {
		if !s.Standard {
			n += s.Bytes
		}
	}
	return n
}

// readJPEG walks the segments up to the end of the image (EOI), skipping the entropy-coded
// data after each start of scan
func (info *imageInfo) readJPEG(data []byte) {
	i := 2
	for i+4 <= len(data) {
		if data[i] != 0xff {
			return
		}
		marker := data[i+1]
		if marker == 0xff {
			// fill byte
			i++
			continue
		}
		if marker == 0xd9 {
			info.Trailing = len(data) - i - 2
			return
		}
		if marker == 0x01 || marker >= 0xd0 && marker <= 0xd7 {
			i += 2
			continue
		}
		length := int(binary.BigEndian.Uint16(data[i+2:]))
		if length < 2 || i+2+length > len(data) {
			return
		}
		payload := data[i+4 : i+2+length]
		if marker >= 0xe0 && marker <= 0xef {
			id := segmentID(payload)
			kind := fmt.Sprintf("APP%d", marker-0xe0)
			if id != "" {
				kind += " " + id
			}
			info.addSegment(kind, length+2, isStandardAPP(marker, id))
			if marker == 0xe1 && id == "Exif" && len(payload) > 6 && info.Camera == "" {
				info.Camera = exifCamera(payload[6:])
			}
		}
		i += 2 + length
		if marker == 0xda {
			i = skipEntropyData(data, i)
		}
	}
}

// skipEntropyData returns the position of the next marker after entropy-coded data: 0xff
// followed by 0x00 is a data byte, restart markers belong to the data
func skipEntropyData(data []byte, i int) int {
	for i+1 < len(data) {
		if data[i] == 0xff && data[i+1] != 0x00 && (data[i+1] < 0xd0 || data[i+1] > 0xd7) {
			return i
		}
		i++
	}
	return len(data)
}

// segmentID is the zero-terminated identifier at the start of an APPn segment, like "Exif" or
// "ICC_PROFILE", or "" if there is none
func segmentID(payload []byte) string {
	end := bytes.IndexByte(payload[:min(len(payload), 33)], 0)
	if end <= 0 {
		return ""
	}
	for _, c := range payload[:end] {
		if c < 0x20 || c > 0x7e {
			return ""
		}
	}
	return string(payload[:end])
}

func isStandardAPP(marker byte, id string) bool {
	switch {
	case marker == 0xe0 && (id == "JFIF" || id == "JFXX"):
	case marker == 0xe1 && (id == "Exif" || strings.HasPrefix(id, "http://ns.adobe.com/")):
	case marker == 0xe2 && id == "ICC_PROFILE":
	case marker == 0xed && strings.HasPrefix(id, "Photoshop"):
	case marker == 0xee && strings.HasPrefix(id, "Adobe"):
	default:
		return false
	}
	return true
}

// exifCamera reads Make (0x010f) and Model (0x0110) of IFD0 of a TIFF structure
func exifCamera(tiff []byte) string {
	if len(tiff) < 8 {
		return ""
	}
	var order binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return ""
	}
	ifd := int(order.Uint32(tiff[4:8]))
	if ifd < 0 || ifd+2 > len(tiff) {
		return ""
	}
	var maker, model string
	for n := range int(order.Uint16(tiff[ifd:])) {
		e := ifd + 2 + 12*n
		if e+12 > len(tiff) {
			break
		}
		tag := order.Uint16(tiff[e:])
		if (tag != 0x010f && tag != 0x0110) || order.Uint16(tiff[e+2:]) != 2 {
			continue
		}
		count := int(order.Uint32(tiff[e+4:]))
		var value []byte
		if count <= 4 {
			value = tiff[e+8 : e+8+count]
		} else if off := int(order.Uint32(tiff[e+8:])); off >= 0 && off+count <= len(tiff) && count <= 256 {
			value = tiff[off : off+count]
		}
		s := strings.TrimSpace(strings.TrimRight(string(value), "\x00"))
		if tag == 0x010f {
			maker = s
		} else {
			model = s
		}
	}
	if model != "" && maker != "" && !strings.HasPrefix(model, maker) {
		return maker + " " + model
	}
	return strings.TrimSpace(maker + " " + model)
}

// readPNG walks the chunks up to IEND
func (info *imageInfo) readPNG(data []byte) {
	i := 8
	for i+12 <= len(data) {
		length := int(binary.BigEndian.Uint32(data[i:]))
		typ := string(data[i+4 : i+8])
		end := i + 12 + length
		if length < 0 || end > len(data) {
			return
		}
		switch typ {
		case "IEND":
			info.Trailing = len(data) - end
			return
		case "IHDR", "PLTE", "IDAT", "tRNS", "gAMA", "cHRM", "sRGB", "pHYs", "bKGD", "sBIT", "acTL", "fcTL", "fdAT":
		default:
			standard := typ == "iCCP" || typ == "tIME" || ((typ == "tEXt" || typ == "iTXt" || typ == "zTXt" || typ == "eXIf") && length < imageExtraThreshold)
			info.addSegment("PNG chunk "+typ, length+12, standard)
		}
		i = end
	}
}

// describe returns the lines read_resource shows about an image it scaled to w x h
func (info *imageInfo) describe(w, h int) string {
	var b strings.Builder
	if info.Width > 0 {
		fmt.Fprintf(&b, "original: %d x %d px", info.Width, info.Height)
		if w != info.Width || h != info.Height {
			fmt.Fprintf(&b, ", shown scaled to %d x %d px", w, h)
		}
		b.WriteString("\n")
	}
	if info.Camera != "" {
		fmt.Fprintf(&b, "camera: %s\n", info.Camera)
	}
	var segments []string
	for _, s := range info.Segments {
		if s.Count > 1 {
			segments = append(segments, fmt.Sprintf("%s x%d (%s)", s.Kind, s.Count, formatBytes(s.Bytes)))
		} else {
			segments = append(segments, fmt.Sprintf("%s (%s)", s.Kind, formatBytes(s.Bytes)))
		}
	}
	if len(segments) > 0 {
		fmt.Fprintf(&b, "metadata: %s\n", strings.Join(segments, ", "))
	}
	if info.Trailing > 0 {
		fmt.Fprintf(&b, "after the end of the image: %s\n", formatBytes(info.Trailing))
	}
	if extra := info.extraBytes(); extra >= imageExtraThreshold {
		fmt.Fprintf(&b, "note: the file holds %s of data that is not in the picture you see, for example raw sensor "+
			"or thermal data, a depth map or another image. download_resource gets the original file.\n", formatBytes(extra))
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func formatBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
