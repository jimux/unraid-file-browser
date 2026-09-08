package meta

import "unraid-filebrowser/internal/types"

// Extension sets. These are the crawler's whole scope decision: a file whose
// lowercase extension is listed gets its header read, everything else is
// never opened. Keep them to formats the extractors actually parse.
var (
	imageExts = []string{
		"jpg", "jpeg", "jpe", "tif", "tiff", "png", "heic", "heif", "avif",
		"rw2", "cr2", "cr3", "nef", "nrw", "arw", "orf", "dng", "raf", "pef", "crw",
	}
	videoExts = []string{
		"mkv", "mp4", "m4v", "mov", "avi", "webm", "ts", "m2ts", "mts", "wmv",
		"mpg", "mpeg", "vob", "flv", "3gp", "ogv",
	}
	audioExts = []string{
		"mp3", "flac", "m4a", "m4b", "ogg", "oga", "opus", "wav", "aiff", "aif",
		"wma", "ape", "dsf", "aac", "mka",
	}
)

// Catalog describes every searchable field for the UI: categories with their
// extensions and typed field definitions carrying friendly labels. The
// "common" category has nil Extensions and covers the columns every indexed
// file has. Values of enum fields are the exact strings stored in the index.
func Catalog() []types.MetaCategory {
	return []types.MetaCategory{
		{
			ID: "common", Label: "Any file", Extensions: nil,
			Fields: []types.MetaFieldDef{
				{Key: "name", Label: "File name", Type: "text"},
				{Key: "ext", Label: "Extension", Type: "text"},
				{Key: "size", Label: "Size", Type: "bytes"},
				{Key: "mtime", Label: "Modified", Type: "date"},
				{Key: "mime", Label: "Media class", Type: "enum", Values: []types.MetaEnumValue{
					{Value: "image", Label: "Image"}, {Value: "video", Label: "Video"},
					{Value: "audio", Label: "Audio"}, {Value: "text", Label: "Text"},
					{Value: "application", Label: "Application"}, {Value: "archive", Label: "Archive"},
				}},
			},
		},
		{
			ID: "image", Label: "Photo", Extensions: catalogExts(KindImage),
			Fields: []types.MetaFieldDef{
				{Key: "image.cameraMake", Label: "Camera make", Type: "text"},
				{Key: "image.cameraModel", Label: "Camera model", Type: "text"},
				{Key: "image.lens", Label: "Lens", Type: "text"},
				{Key: "image.iso", Label: "ISO", Type: "number"},
				{Key: "image.fNumber", Label: "Aperture (f/)", Type: "number"},
				{Key: "image.exposureTime", Label: "Exposure time", Type: "number", Unit: "s"},
				{Key: "image.focalLength", Label: "Focal length", Type: "number", Unit: "mm"},
				{Key: "image.dateTaken", Label: "Date taken", Type: "date"},
				{Key: "image.width", Label: "Width", Type: "number", Unit: "px"},
				{Key: "image.height", Label: "Height", Type: "number", Unit: "px"},
				{Key: "image.orientation", Label: "Orientation", Type: "text"},
				{Key: "image.software", Label: "Software", Type: "text"},
				{Key: "image.gps", Label: "Has GPS location", Type: "bool", Values: yesNo},
				{Key: "image.gpsLat", Label: "Latitude", Type: "number", Unit: "°"},
				{Key: "image.gpsLon", Label: "Longitude", Type: "number", Unit: "°"},
			},
		},
		{
			ID: "video", Label: "Video", Extensions: catalogExts(KindVideo),
			Fields: []types.MetaFieldDef{
				{Key: "video.codec", Label: "Video codec", Type: "enum", Values: []types.MetaEnumValue{
					{Value: "h264", Label: "H.264 / AVC"}, {Value: "hevc", Label: "H.265 / HEVC"},
					{Value: "av1", Label: "AV1"}, {Value: "vp9", Label: "VP9"}, {Value: "vp8", Label: "VP8"},
					{Value: "mpeg4", Label: "MPEG-4 Part 2 (DivX/Xvid)"}, {Value: "mpeg2video", Label: "MPEG-2"},
					{Value: "mpeg1video", Label: "MPEG-1"}, {Value: "vc1", Label: "VC-1"},
					{Value: "wmv3", Label: "Windows Media Video 9"}, {Value: "prores", Label: "Apple ProRes"},
					{Value: "dnxhd", Label: "Avid DNxHD"}, {Value: "mjpeg", Label: "Motion JPEG"},
				}},
				{Key: "video.width", Label: "Width", Type: "number", Unit: "px"},
				{Key: "video.height", Label: "Height", Type: "number", Unit: "px"},
				{Key: "video.hdr", Label: "HDR", Type: "enum", Values: []types.MetaEnumValue{
					{Value: "hdr10", Label: "HDR10 (PQ)"}, {Value: "hlg", Label: "HLG"},
					{Value: "dolbyvision", Label: "Dolby Vision"}, {Value: "none", Label: "No HDR"},
				}},
				{Key: "video.bitDepth", Label: "Bit depth", Type: "number", Unit: "bit"},
				{Key: "video.fps", Label: "Frame rate", Type: "number", Unit: "fps"},
				{Key: "video.durationSec", Label: "Duration", Type: "number", Unit: "s"},
				{Key: "video.bitrate", Label: "Bitrate", Type: "number", Unit: "bit/s"},
				{Key: "video.container", Label: "Container", Type: "text"},
				{Key: "video.audioCodec", Label: "Audio codec", Type: "enum", Values: []types.MetaEnumValue{
					{Value: "ac3", Label: "Dolby Digital (AC-3)"}, {Value: "eac3", Label: "Dolby Digital Plus (E-AC-3)"},
					{Value: "truehd", Label: "Dolby TrueHD"}, {Value: "dts", Label: "DTS"},
					{Value: "aac", Label: "AAC"}, {Value: "mp3", Label: "MP3"}, {Value: "mp2", Label: "MP2"},
					{Value: "opus", Label: "Opus"}, {Value: "vorbis", Label: "Vorbis"}, {Value: "flac", Label: "FLAC"},
					{Value: "pcm_s16le", Label: "PCM 16-bit"}, {Value: "pcm_s24le", Label: "PCM 24-bit"},
					{Value: "wmav2", Label: "Windows Media Audio"},
				}},
				{Key: "video.audioChannels", Label: "Audio channels", Type: "number"},
				{Key: "video.audioLang", Label: "Audio language", Type: "text"},
				{Key: "video.subtitleLang", Label: "Subtitle language", Type: "text"},
			},
		},
		{
			ID: "audio", Label: "Music", Extensions: catalogExts(KindAudio),
			Fields: []types.MetaFieldDef{
				{Key: "audio.artist", Label: "Artist", Type: "text"},
				{Key: "audio.albumArtist", Label: "Album artist", Type: "text"},
				{Key: "audio.album", Label: "Album", Type: "text"},
				{Key: "audio.title", Label: "Title", Type: "text"},
				{Key: "audio.year", Label: "Year", Type: "number"},
				{Key: "audio.genre", Label: "Genre", Type: "text"},
				{Key: "audio.track", Label: "Track number", Type: "number"},
				{Key: "audio.codec", Label: "Codec", Type: "enum", Values: []types.MetaEnumValue{
					{Value: "mp3", Label: "MP3"}, {Value: "flac", Label: "FLAC"}, {Value: "aac", Label: "AAC"},
					{Value: "alac", Label: "Apple Lossless"}, {Value: "vorbis", Label: "Vorbis"}, {Value: "opus", Label: "Opus"},
					{Value: "pcm_s16le", Label: "PCM 16-bit"}, {Value: "pcm_s24le", Label: "PCM 24-bit"},
					{Value: "wmav2", Label: "Windows Media Audio"}, {Value: "ape", Label: "Monkey's Audio"},
					{Value: "dsd", Label: "DSD"},
				}},
				{Key: "audio.bitrate", Label: "Bitrate", Type: "number", Unit: "bit/s"},
				{Key: "audio.sampleRate", Label: "Sample rate", Type: "number", Unit: "Hz"},
				{Key: "audio.channels", Label: "Channels", Type: "number"},
				{Key: "audio.durationSec", Label: "Duration", Type: "number", Unit: "s"},
			},
		},
		{
			ID: "package", Label: "Package", Extensions: catalogExts(KindPackage),
			Fields: []types.MetaFieldDef{
				{Key: "package.name", Label: "Package name", Type: "text"},
				{Key: "package.version", Label: "Version", Type: "text"},
				{Key: "package.release", Label: "Release", Type: "text"},
				{Key: "package.arch", Label: "Architecture", Type: "text"},
				{Key: "package.summary", Label: "Summary", Type: "text"},
				{Key: "package.section", Label: "Section / group", Type: "text"},
				{Key: "package.maintainer", Label: "Maintainer", Type: "text"},
				{Key: "package.depends", Label: "Depends on", Type: "text"},
				{Key: "package.installedSize", Label: "Installed size", Type: "bytes"},
				{Key: "package.license", Label: "License", Type: "text"},
				{Key: "package.vendor", Label: "Vendor", Type: "text"},
				{Key: "package.format", Label: "Format", Type: "enum", Values: []types.MetaEnumValue{
					{Value: "deb", Label: "Debian (.deb)"}, {Value: "rpm", Label: "RPM (.rpm)"},
				}},
			},
		},
	}
}

var yesNo = []types.MetaEnumValue{{Value: "yes", Label: "Yes"}, {Value: "no", Label: "No"}}

// FieldDefs flattens the catalog into a key → definition map, including the
// common keys.
func FieldDefs() map[string]types.MetaFieldDef {
	m := map[string]types.MetaFieldDef{}
	for _, c := range Catalog() {
		for _, f := range c.Fields {
			m[f.Key] = f
		}
	}
	return m
}
