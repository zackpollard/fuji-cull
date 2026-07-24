package cull

import (
	"crypto/sha1"
	"encoding/hex"
	"log"
	"sort"
	"strings"

	"github.com/zack/fuji-tools/internal/photo"
)

// The canonical shot key is the device-INDEPENDENT identity used for cross-device
// decision sync. A shot's backend-local ID ("SLOT 1/DCIM/151_FUJI/DSCF0001" on
// desktop, "DCIM/151_FUJI/DSCF0001" on the iOS PTP path) differs per backend, so
// decisions keyed by ID do not line up across devices. The canonical key is
// "<Folder>/<Base>" ("151_FUJI/DSCF0001"), which every backend produces
// identically. We add it as a SEPARATE field and never rewrite ID (thumb/fetch
// caches key on ID, and dual-slot twins must keep distinct IDs so cat.Index and
// counts() don't collide).

// assignCanonicalKeys sets s.CanonicalKey on every shot and returns the two
// reverse indexes: canonical (legacyID -> canonicalKey, 1:1) and legacy
// (canonicalKey -> []legacyID, 1:N — a dual-slot backup twin maps to both IDs).
//
// Dual-card overflow guard: a Fuji body in sequential/overflow mode fills SLOT 1
// then continues on SLOT 2 with its own NNN_FUJI/DSCF counter, so two cards can
// hold "151_FUJI/DSCF0001" as DIFFERENT exposures. Grouping those under one key
// would let a keep on one and a reject on the other collide. So when shots share
// (Folder,Base) but differ in content (size/date), each gets a "#<fingerprint>"
// suffix keyed to its slot prefix, keeping them separate; identical twins (true
// backup mode) share the plain key so a decision mirrors across cards.
func assignCanonicalKeys(shots []*photo.Shot) (canonical map[string]string, legacy map[string][]string) {
	canonical = make(map[string]string, len(shots))
	legacy = make(map[string][]string, len(shots))

	// group by the plain "<Folder>/<Base>" key
	groups := map[string][]*photo.Shot{}
	for _, s := range shots {
		plain := s.Folder + "/" + s.Base
		groups[plain] = append(groups[plain], s)
	}

	for plain, group := range groups {
		if len(group) == 1 {
			group[0].CanonicalKey = plain
		} else if sameContent(group) {
			// true backup mode: identical frame on >1 card — share the register
			for _, s := range group {
				s.CanonicalKey = plain
			}
		} else {
			// overflow: same number, different exposures — disambiguate by slot
			for _, s := range group {
				s.CanonicalKey = plain + "#" + slotFingerprint(s.CameraDir)
			}
			log.Printf("sync: dual-card overflow at %s across %d cards — keys disambiguated", plain, len(group))
		}
	}

	for _, s := range shots {
		canonical[s.ID] = s.CanonicalKey
		legacy[s.CanonicalKey] = append(legacy[s.CanonicalKey], s.ID)
	}
	// stable order so Legacy[k] projection is deterministic
	for k := range legacy {
		sort.Strings(legacy[k])
	}
	return canonical, legacy
}

// sameContent reports whether every shot in the group is byte-identical in size
// and capture date (the backup-mode signature). Any difference means they are
// distinct exposures that happen to share a frame number.
func sameContent(group []*photo.Shot) bool {
	first := group[0]
	for _, s := range group[1:] {
		if s.Date != first.Date || len(s.Sizes) != len(first.Sizes) {
			return false
		}
		for ext, sz := range first.Sizes {
			if s.Sizes[ext] != sz {
				return false
			}
		}
	}
	return true
}

// slotFingerprint hashes the slot/DCIM prefix of a CameraDir (everything before
// the NNN_FUJI folder) into a short stable token, so overflow twins on SLOT 1 vs
// SLOT 2 get distinct — but deterministic — canonical keys on every device.
func slotFingerprint(cameraDir string) string {
	prefix := cameraDir
	if i := photo.FolderRe.FindStringIndex(cameraDir); i != nil {
		prefix = cameraDir[:i[0]]
	}
	prefix = strings.Trim(prefix, "/ ")
	sum := sha1.Sum([]byte(prefix))
	return hex.EncodeToString(sum[:])[:6]
}

// slugify sanitizes a camera identity into a filesystem/URL-safe slug: only
// [A-Za-z0-9-] survive, everything else becomes '-'.
func slugify(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			return r
		default:
			return '-'
		}
	}, s)
}

// canonicalizeLegacyKey converts a stored v1 decision key (backend-local Shot.ID)
// into its canonical form by string parsing alone — no catalog, so it is immune
// to which discovery path a run took and works before discovery completes. It
// takes the last two "/"-segments when the penultimate one is a Fuji folder:
//
//	"DCIM/151_FUJI/DSCF0001"        -> "151_FUJI/DSCF0001"
//	"SLOT 1/DCIM/151_FUJI/DSCF0001" -> "151_FUJI/DSCF0001"
//	"151_FUJI/DSCF0001"             -> "151_FUJI/DSCF0001" (idempotent)
//	"151_FUJI/DSCF0001#abc123"      -> unchanged (already disambiguated)
//
// ok is false for a key that does not end in a recognizable "<NNN_FUJI>/<base>"
// (leave such keys legacy-only rather than mis-canonicalizing them).
func canonicalizeLegacyKey(oldID string) (canonical string, ok bool) {
	// an already-disambiguated overflow key is canonical as-is
	if i := strings.LastIndex(oldID, "#"); i >= 0 {
		head := oldID[:i]
		if _, valid := canonicalizeLegacyKey(head); valid {
			return oldID, true
		}
		return "", false
	}
	parts := strings.Split(oldID, "/")
	if len(parts) < 2 {
		return "", false
	}
	folder := parts[len(parts)-2]
	base := parts[len(parts)-1]
	if base == "" || !photo.FolderRe.MatchString(folder) {
		return "", false
	}
	return folder + "/" + base, true
}
