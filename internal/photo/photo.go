// Package photo holds the shared file/shot types used across fuji-tools.
package photo

import (
	"path/filepath"
	"regexp"
	"strings"
)

var (
	// FolderRe matches Fuji DCIM subfolders like "151_FUJI".
	FolderRe = regexp.MustCompile(`\b\d{3}_FUJI\b`)
	// FileRe matches Fuji media files like "DSCF0001.JPG".
	FileRe = regexp.MustCompile(`DSCF\d+\.(JPG|MOV|RAF|MP4)`)
)

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
	ID        string            // backend-local id "<CameraDir>/<Base>" — stable WITHIN a device; used for fetch/thumb/cache keys. NOT portable across backends.
	// CanonicalKey is the device-INDEPENDENT sync key: "<Folder>/<Base>", e.g.
	// "151_FUJI/DSCF0001". Unlike ID it drops the backend-specific slot/DCIM
	// prefix so the same physical frame gets the same key on every backend. A
	// "#<fingerprint>" suffix disambiguates dual-card overflow twins (§ sync).
	CanonicalKey string
	CameraDir string            // dir relative to the camera root, e.g. "SLOT 1/DCIM/151_FUJI"
	Folder    string            // base folder name, e.g. "151_FUJI" (used for dest layout)
	Base      string            // "DSCF0001"
	Date      string            // capture day "2006-01-02" for timeline grouping; "" unknown
	Kind      string            // "photo" | "video"
	Files     map[string]string // upper-case ext (without dot) -> filename, e.g. "JPG" -> "DSCF0001.JPG"
	Sizes     map[string]int64  // upper-case ext -> size in bytes
	ObjectIDs map[string]string // upper-case ext -> MTP object ID (cli backend; enables get-id)
}

// DisplayExt returns the extension of the file used for on-screen preview.
func (s *Shot) DisplayExt() string {
	for _, ext := range []string{"JPG", "RAF", "MOV", "MP4"} {
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

// SplitMedia parses "DSCF0001.JPG" into base and upper-case ext; ok=false if not a Fuji media file.
func SplitMedia(name string) (base, ext string, ok bool) {
	upper := strings.ToUpper(name)
	if !FileRe.MatchString(upper) {
		return "", "", false
	}
	e := strings.TrimPrefix(strings.ToUpper(filepath.Ext(name)), ".")
	b := strings.TrimSuffix(name, filepath.Ext(name))
	return b, e, true
}
