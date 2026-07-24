package cull

import (
	"reflect"
	"testing"
)

func TestCanonicalizeLegacyKey(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		// the real on-device iPad format (verified: pulled from the live camera session)
		{"SLOT 1/DCIM/151_FUJI/DSCF8558", "151_FUJI/DSCF8558", true},
		// the iOS PTP-path shape (slot dropped)
		{"DCIM/151_FUJI/DSCF0001", "151_FUJI/DSCF0001", true},
		// already canonical — idempotent
		{"151_FUJI/DSCF0001", "151_FUJI/DSCF0001", true},
		// dual-card overflow key already disambiguated — left as-is
		{"151_FUJI/DSCF0001#a1b2c3", "151_FUJI/DSCF0001#a1b2c3", true},
		// deeper nesting still resolves to the last folder/base
		{"whatever/prefix/302_FUJI/DSCF9999", "302_FUJI/DSCF9999", true},
		// not a Fuji folder in the penultimate slot -> not canonicalizable
		{"foo/bar", "", false},
		// single segment -> not canonicalizable
		{"DSCF0001", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := canonicalizeLegacyKey(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("canonicalizeLegacyKey(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestCanonicalizeLegacyKeyIdempotent(t *testing.T) {
	// canonicalizing an already-canonical key must be a fixed point
	for _, k := range []string{"151_FUJI/DSCF8558", "302_FUJI/DSCF0001#a1b2c3"} {
		once, ok := canonicalizeLegacyKey(k)
		if !ok || once != k {
			t.Fatalf("first pass %q -> (%q,%v)", k, once, ok)
		}
		twice, ok := canonicalizeLegacyKey(once)
		if !ok || twice != once {
			t.Errorf("not idempotent: %q -> %q -> %q", k, once, twice)
		}
	}
}

// A single card: every canonical key is plain "<Folder>/<Base>" and the reverse
// indexes round-trip.
func TestBuildCatalogCanonicalSingleCard(t *testing.T) {
	cat := buildCatalog([]listing{
		{Dir: "SLOT 1/DCIM/151_FUJI", Folder: "151_FUJI", Name: "DSCF0001.JPG", Size: 100, Date: "2026-07-01"},
		{Dir: "SLOT 1/DCIM/151_FUJI", Folder: "151_FUJI", Name: "DSCF0002.JPG", Size: 200, Date: "2026-07-01"},
	})
	want := map[string]string{
		"SLOT 1/DCIM/151_FUJI/DSCF0001": "151_FUJI/DSCF0001",
		"SLOT 1/DCIM/151_FUJI/DSCF0002": "151_FUJI/DSCF0002",
	}
	if !reflect.DeepEqual(cat.Canonical, want) {
		t.Errorf("Canonical = %v, want %v", cat.Canonical, want)
	}
	// every shot carries its canonical key, and Legacy is the exact inverse
	for _, s := range cat.Shots {
		if s.CanonicalKey != want[s.ID] {
			t.Errorf("shot %s CanonicalKey=%q want %q", s.ID, s.CanonicalKey, want[s.ID])
		}
		if got := cat.LegacyOf(s.CanonicalKey); !reflect.DeepEqual(got, []string{s.ID}) {
			t.Errorf("LegacyOf(%q) = %v, want [%q]", s.CanonicalKey, got, s.ID)
		}
	}
}

// Dual-card BACKUP (same frame byte-identical on both slots): the twins SHARE one
// canonical key, so a decision on either mirrors to both. Legacy[key] has both IDs.
func TestBuildCatalogCanonicalBackupTwins(t *testing.T) {
	cat := buildCatalog([]listing{
		{Dir: "SLOT 1/DCIM/151_FUJI", Folder: "151_FUJI", Name: "DSCF0001.JPG", Size: 100, Date: "2026-07-01"},
		{Dir: "SLOT 2/DCIM/151_FUJI", Folder: "151_FUJI", Name: "DSCF0001.JPG", Size: 100, Date: "2026-07-01"},
	})
	ids := cat.LegacyOf("151_FUJI/DSCF0001")
	if len(ids) != 2 {
		t.Fatalf("backup twins should share one canonical key with 2 legacy IDs, got %v", ids)
	}
	for _, s := range cat.Shots {
		if s.CanonicalKey != "151_FUJI/DSCF0001" {
			t.Errorf("twin %s should share key, got %q", s.ID, s.CanonicalKey)
		}
	}
}

// Dual-card OVERFLOW (same frame number, DIFFERENT exposures on SLOT 1 vs SLOT 2):
// keys are disambiguated with a stable per-slot suffix so a keep on one can't
// collide with a reject on the other.
func TestBuildCatalogCanonicalOverflowDisambiguates(t *testing.T) {
	cat := buildCatalog([]listing{
		{Dir: "SLOT 1/DCIM/151_FUJI", Folder: "151_FUJI", Name: "DSCF0001.JPG", Size: 100, Date: "2026-07-01"},
		{Dir: "SLOT 2/DCIM/151_FUJI", Folder: "151_FUJI", Name: "DSCF0001.JPG", Size: 999, Date: "2026-07-02"},
	})
	keys := map[string]bool{}
	for _, s := range cat.Shots {
		keys[s.CanonicalKey] = true
		if s.CanonicalKey == "151_FUJI/DSCF0001" {
			t.Errorf("overflow twin %s must be disambiguated, got plain key", s.ID)
		}
	}
	if len(keys) != 2 {
		t.Errorf("overflow twins must get 2 distinct keys, got %v", keys)
	}
	// deterministic: same input -> same suffixes on a rebuild
	cat2 := buildCatalog([]listing{
		{Dir: "SLOT 1/DCIM/151_FUJI", Folder: "151_FUJI", Name: "DSCF0001.JPG", Size: 100, Date: "2026-07-01"},
		{Dir: "SLOT 2/DCIM/151_FUJI", Folder: "151_FUJI", Name: "DSCF0001.JPG", Size: 999, Date: "2026-07-02"},
	})
	if !reflect.DeepEqual(cat.Canonical, cat2.Canonical) {
		t.Errorf("overflow disambiguation not deterministic:\n%v\n%v", cat.Canonical, cat2.Canonical)
	}
}
