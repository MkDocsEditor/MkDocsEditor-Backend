package backend

import (
	"mime"
	"path"
	"strings"
)

// Kinds of resources, see docs/references.md section 9
const (
	ResourceKindImage   = "image"
	ResourceKindVideo   = "video"
	ResourceKindAudio   = "audio"
	ResourceKindPDF     = "pdf"
	ResourceKindText    = "text"
	ResourceKindArchive = "archive"
	ResourceKindOther   = "other"

	mimeTypeUnknown = "application/octet-stream"
)

// MIME types of common wiki files. The system's mime.types differs between hosts and is often
// missing in containers, so the common types do not depend on it.
var mimeTypes = map[string]string{
	".apng": "image/apng",
	".avif": "image/avif",
	".bmp":  "image/bmp",
	".gif":  "image/gif",
	".heic": "image/heic",
	".ico":  "image/vnd.microsoft.icon",
	".jpeg": "image/jpeg",
	".jpg":  "image/jpeg",
	".png":  "image/png",
	".svg":  "image/svg+xml",
	".tif":  "image/tiff",
	".tiff": "image/tiff",
	".webp": "image/webp",
	".avi":  "video/x-msvideo",
	".m4v":  "video/mp4",
	".mkv":  "video/x-matroska",
	".mov":  "video/quicktime",
	".mp4":  "video/mp4",
	".webm": "video/webm",
	".flac": "audio/flac",
	".m4a":  "audio/mp4",
	".mp3":  "audio/mpeg",
	".oga":  "audio/ogg",
	".ogg":  "audio/ogg",
	".opus": "audio/opus",
	".wav":  "audio/wav",
	".pdf":  "application/pdf",
	".csv":  "text/csv",
	".css":  "text/css",
	".htm":  "text/html",
	".html": "text/html",
	".ini":  "text/plain",
	".js":   "text/javascript",
	".json": "application/json",
	".log":  "text/plain",
	".md":   "text/markdown",
	".toml": "application/toml",
	".txt":  "text/plain",
	".xml":  "application/xml",
	".yaml": "application/yaml",
	".yml":  "application/yaml",
	".7z":   "application/x-7z-compressed",
	".gz":   "application/gzip",
	".rar":  "application/vnd.rar",
	".tar":  "application/x-tar",
	".tgz":  "application/gzip",
	".zip":  "application/zip",
	".doc":  "application/msword",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".odp":  "application/vnd.oasis.opendocument.presentation",
	".ods":  "application/vnd.oasis.opendocument.spreadsheet",
	".odt":  "application/vnd.oasis.opendocument.text",
	".ppt":  "application/vnd.ms-powerpoint",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".xls":  "application/vnd.ms-excel",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
}

// mimeTypeOf returns the MIME type of a file name, from its extension
func mimeTypeOf(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if t, ok := mimeTypes[ext]; ok {
		return t
	}
	if t := mime.TypeByExtension(ext); t != "" {
		if mediaType, _, err := mime.ParseMediaType(t); err == nil {
			return mediaType
		}
	}
	return mimeTypeUnknown
}

// resourceKind groups MIME types for icons and pickers
func resourceKind(mimeType string) string {
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		return ResourceKindImage
	case strings.HasPrefix(mimeType, "video/"):
		return ResourceKindVideo
	case strings.HasPrefix(mimeType, "audio/"):
		return ResourceKindAudio
	case mimeType == "application/pdf":
		return ResourceKindPDF
	case strings.HasPrefix(mimeType, "text/"), mimeType == "application/json", mimeType == "application/xml",
		mimeType == "application/yaml", mimeType == "application/toml":
		return ResourceKindText
	case mimeType == "application/zip", mimeType == "application/gzip", mimeType == "application/x-tar",
		mimeType == "application/x-7z-compressed", mimeType == "application/vnd.rar":
		return ResourceKindArchive
	}
	return ResourceKindOther
}
