// Package photo holds the shared file/shot types used across fuji-tools.
package photo

import (
	"path/filepath"
	"regexp"
	"strings"
)

// Names are matched against parsed directory entries, never scraped out of raw
// tool output: an `ls` line carries an object ID and a size too, and those
// columns are shaped exactly like the names below.
var (
	// A DCF directory is three digits plus five free characters: "151_FUJI"
	// (Fuji), "100MSDCF" (Sony), "100CANON", "100_PANA".
	dcfFolderRe = regexp.MustCompile(`^\d{3}[A-Z0-9_]{5}$`)
	// A Sony body in MTP mode exposes no DCIM tree at all — media sits in
	// date-named folders at the storage root ("2026-08-12").
	dateFolderRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	// A DCF basename is up to five leading characters then a frame number:
	// "DSCF0001" (Fuji), "DSC01764"/"_DSC0001"/"C0001" (Sony), "IMG_0001".
	mediaFileRe = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,4}\d{3,5}\.(?:JPG|JPEG|MOV|MP4|RAF|ARW)$`)
)

// RawExts are the raw extensions the pipeline knows how to pair with a JPG,
// pull an embedded preview out of, and upload.
var RawExts = []string{"RAF", "ARW"}

// IsMediaFolder reports whether a parsed directory name is one the camera
// stores media in — either DCF layout or a Sony MTP date folder.
func IsMediaFolder(name string) bool {
	up := strings.ToUpper(name)
	if dateFolderRe.MatchString(up) {
		return true
	}
	// A DCF name's five free characters always carry a letter or underscore
	// ("_FUJI", "MSDCF", "CANON"). Requiring one keeps an all-digit token —
	// an object ID, a byte count — from passing as a folder name.
	return dcfFolderRe.MatchString(up) &&
		strings.ContainsAny(up[3:], "ABCDEFGHIJKLMNOPQRSTUVWXYZ_")
}

// MediaFolderPrefix returns the part of a camera dir that precedes its media
// folder segment ("SLOT 1/DCIM/151_FUJI" -> "SLOT 1/DCIM"), or the whole path
// when it holds no recognizable media folder.
func MediaFolderPrefix(cameraDir string) string {
	parts := strings.Split(cameraDir, "/")
	for i := len(parts) - 1; i >= 0; i-- {
		if IsMediaFolder(parts[i]) {
			return strings.Join(parts[:i], "/")
		}
	}
	return cameraDir
}

// FileEntry is one media file moving through the import pipeline.
type FileEntry struct {
	Folder    string // e.g. "151_FUJI"
	Name      string // e.g. "DSCF0001.JPG"
	LocalPath string // absolute path on NAS
	Size      int64  // populated after pull
	SHA1      string // hex, populated after hash
	SHA1B64   string // base64, for Immich bulk-check
	AssetID   string // Immich asset ID after upload
}

func (f FileEntry) CameraPath() string { return f.Folder + "/" + f.Name }

// Shot groups files that belong to one exposure: a RAF+JPG pair, or a video.
type Shot struct {
	ID string // backend-local id "<CameraDir>/<Base>" — stable WITHIN a device; used for fetch/thumb/cache keys. NOT portable across backends.
	// CanonicalKey is the device-INDEPENDENT sync key: "<Folder>/<Base>", e.g.
	// "151_FUJI/DSCF0001". Unlike ID it drops the backend-specific slot/DCIM
	// prefix so the same physical frame gets the same key on every backend. A
	// "#<fingerprint>" suffix disambiguates dual-card overflow twins (§ sync).
	CanonicalKey string
	CameraDir    string // dir relative to the camera root, e.g. "SLOT 1/DCIM/151_FUJI"
	Folder       string // base folder name, e.g. "151_FUJI" (used for dest layout)
	Base         string // "DSCF0001"
	Date         string // capture day "2006-01-02" for timeline grouping; "" unknown
	// Taken is the camera's own timestamp for the file ("20260714T151530").
	// Kept at full precision because it is the cheapest proof that the bytes
	// we downloaded are the file we asked for: a rebound handle of identical
	// size passes every other check.
	Taken     string
	Kind      string            // "photo" | "video"
	Files     map[string]string // upper-case ext (without dot) -> filename, e.g. "JPG" -> "DSCF0001.JPG"
	Sizes     map[string]int64  // upper-case ext -> size in bytes
	ObjectIDs map[string]string // upper-case ext -> MTP object ID (cli backend; enables get-id)
}

// DisplayExt returns the extension of the file used for on-screen preview.
func (s *Shot) DisplayExt() string {
	for _, ext := range []string{"JPG", "JPEG", "RAF", "ARW", "MOV", "MP4"} {
		if _, ok := s.Files[ext]; ok {
			return ext
		}
	}
	return ""
}

// RawExt returns the shot's raw extension ("RAF", "ARW"), or "" when it has
// none. A raw-only shot has no JPG to display, so it is the one that has to
// pull the raw and extract its embedded preview.
func (s *Shot) RawExt() string {
	for _, ext := range RawExts {
		if _, ok := s.Files[ext]; ok {
			return ext
		}
	}
	return ""
}

// TotalSize is the sum of all file sizes in the shot.
func (s *Shot) TotalSize() int64 {
	var n int64
	for _, sz := range s.Sizes {
		n += sz
	}
	return n
}

// SafeID converts the shot ID into a filesystem-safe cache filename stem.
func (s *Shot) SafeID() string {
	return strings.NewReplacer("/", "_", " ", "-").Replace(s.ID)
}

// ShotKind classifies an upper-case extension.
func ShotKind(ext string) string {
	switch ext {
	case "MOV", "MP4":
		return "video"
	default:
		return "photo"
	}
}

// SplitMedia parses "DSCF0001.JPG" into base and upper-case ext; ok=false if
// the name is not a camera media file.
func SplitMedia(name string) (base, ext string, ok bool) {
	upper := strings.ToUpper(name)
	if !mediaFileRe.MatchString(upper) {
		return "", "", false
	}
	e := strings.TrimPrefix(strings.ToUpper(filepath.Ext(name)), ".")
	b := strings.TrimSuffix(name, filepath.Ext(name))
	return b, e, true
}
