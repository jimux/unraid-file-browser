package view

import (
	"mime"
	"net/http"
	"path/filepath"
	"strings"
)

// extMime is the extension table used everywhere a MIME type is derived
// without reading the file (directory listings must never open files — a
// 100k-entry share has to list fast). It deliberately prefers "text/plain" for
// source and config files so the SPA offers the text viewer by default.
var extMime = map[string]string{
	// text & code
	".txt": "text/plain", ".text": "text/plain", ".log": "text/plain",
	".md": "text/markdown", ".markdown": "text/markdown",
	".csv": "text/csv", ".tsv": "text/tab-separated-values",
	".json": "application/json", ".ndjson": "application/x-ndjson",
	".xml": "application/xml", ".yaml": "application/yaml", ".yml": "application/yaml",
	".toml": "application/toml", ".ini": "text/plain", ".cfg": "text/plain",
	".conf": "text/plain", ".properties": "text/plain", ".env": "text/plain",
	".html": "text/html", ".htm": "text/html", ".css": "text/css",
	".js": "text/javascript", ".mjs": "text/javascript", ".cjs": "text/javascript",
	".ts": "text/plain", ".tsx": "text/plain", ".jsx": "text/plain",
	".go": "text/plain", ".mod": "text/plain", ".sum": "text/plain",
	".c": "text/plain", ".h": "text/plain", ".cpp": "text/plain", ".hpp": "text/plain",
	".cc": "text/plain", ".cs": "text/plain", ".java": "text/plain",
	".py": "text/plain", ".rb": "text/plain", ".pl": "text/plain", ".php": "text/plain",
	".sh": "text/plain", ".bash": "text/plain", ".zsh": "text/plain", ".fish": "text/plain",
	".sql": "text/plain", ".rs": "text/plain", ".swift": "text/plain", ".kt": "text/plain",
	".lua": "text/plain", ".r": "text/plain", ".m": "text/plain", ".vb": "text/plain",
	".bat": "text/plain", ".ps1": "text/plain", ".diff": "text/plain", ".patch": "text/plain",
	".srt": "text/plain", ".vtt": "text/vtt", ".nfo": "text/plain",

	// images
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".bmp": "image/bmp", ".webp": "image/webp",
	".svg": "image/svg+xml", ".ico": "image/x-icon", ".tif": "image/tiff",
	".tiff": "image/tiff", ".avif": "image/avif", ".heic": "image/heic",

	// audio / video
	".mp3": "audio/mpeg", ".flac": "audio/flac", ".wav": "audio/wav",
	".ogg": "audio/ogg", ".opus": "audio/opus", ".m4a": "audio/mp4",
	".aac": "audio/aac", ".wma": "audio/x-ms-wma",
	".mp4": "video/mp4", ".m4v": "video/mp4", ".mkv": "video/x-matroska",
	".avi": "video/x-msvideo", ".mov": "video/quicktime", ".webm": "video/webm",
	".wmv": "video/x-ms-wmv", ".flv": "video/x-flv", ".mpg": "video/mpeg",
	".mpeg": "video/mpeg", ".m2ts": "video/mp2t", ".mts": "video/mp2t",
	".mpe": "video/mpeg", ".m2v": "video/mpeg", ".ogv": "video/ogg", ".ogm": "video/ogg",
	".3gp": "video/3gpp", ".3g2": "video/3gpp2", ".vob": "video/mpeg", ".divx": "video/x-msvideo",
	".asf": "video/x-ms-asf", ".rm": "video/vnd.rn-realvideo", ".rmvb": "video/vnd.rn-realvideo",
	".f4v": "video/mp4",
	".oga": "audio/ogg", ".m4b": "audio/mp4", ".aif": "audio/aiff", ".aiff": "audio/aiff",
	".alac": "audio/mp4", ".ape": "audio/x-ape", ".wv": "audio/x-wavpack", ".mka": "audio/x-matroska",
	".dsf": "audio/x-dsf",
	// NOTE: ".ts" stays text/plain (TypeScript is far more common on a NAS
	// than raw MPEG-TS); the SPA's extension override still offers to play it.

	// documents
	".pdf": "application/pdf", ".rtf": "application/rtf", ".epub": "application/epub+zip",
	".doc": "application/msword", ".xls": "application/vnd.ms-excel",
	".ppt":  "application/vnd.ms-powerpoint",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",

	// archives & images of disks
	".zip": "application/zip", ".jar": "application/java-archive",
	".war": "application/java-archive", ".cbz": "application/vnd.comicbook+zip",
	".tar": "application/x-tar", ".gz": "application/gzip", ".tgz": "application/gzip",
	".bz2": "application/x-bzip2", ".tbz2": "application/x-bzip2",
	".xz": "application/x-xz", ".txz": "application/x-xz", ".zst": "application/zstd",
	".7z": "application/x-7z-compressed", ".rar": "application/vnd.rar",
	".cbr": "application/vnd.comicbook-rar", ".iso": "application/x-iso9660-image",
	".img": "application/octet-stream", ".cab": "application/vnd.ms-cab-compressed",
	".wim": "application/x-ms-wim", ".dmg": "application/x-apple-diskimage",
	".deb": "application/vnd.debian.binary-package", ".rpm": "application/x-rpm",

	// misc binaries
	".exe": "application/vnd.microsoft.portable-executable",
	".dll": "application/vnd.microsoft.portable-executable",
	".so":  "application/x-sharedlib", ".bin": "application/octet-stream",
	".ttf": "font/ttf", ".otf": "font/otf", ".woff": "font/woff", ".woff2": "font/woff2",
}

// MimeByName derives a MIME type from a file name alone, returning "" when the
// extension is unknown. Never touches the filesystem.
func MimeByName(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if ext == "" {
		return ""
	}
	if m, ok := extMime[ext]; ok {
		return m
	}
	if m := mime.TypeByExtension(ext); m != "" {
		return m
	}
	return ""
}

// SniffMime derives a MIME type from the file name, falling back to content
// sniffing over head (the first 512 bytes are enough for net/http's detector).
func SniffMime(head []byte, name string) string {
	if m := MimeByName(name); m != "" {
		return m
	}
	if len(head) == 0 {
		return ""
	}
	if len(head) > 512 {
		head = head[:512]
	}
	return http.DetectContentType(head)
}
