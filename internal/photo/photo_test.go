package photo

import "testing"

func TestIsMediaFolder(t *testing.T) {
	yes := []string{
		"151_FUJI", // Fuji DCF
		"100MSDCF", // Sony DCF
		"100CANON",
		"100_PANA",
		"2026-08-12", // Sony MTP date folder (no DCIM tree at all)
	}
	no := []string{
		"", "DCIM", "SLOT 1", "PRIVATE", "MISC",
		"DSCF0001.JPG",
		"2026-08",     // not a full date
		"2026-08-123", // too long
	}
	for _, n := range yes {
		if !IsMediaFolder(n) {
			t.Errorf("IsMediaFolder(%q) = false, want true", n)
		}
	}
	for _, n := range no {
		if IsMediaFolder(n) {
			t.Errorf("IsMediaFolder(%q) = true, want false", n)
		}
	}
}

func TestSplitMedia(t *testing.T) {
	cases := []struct {
		name, base, ext string
		ok              bool
	}{
		{name: "DSCF0001.JPG", base: "DSCF0001", ext: "JPG", ok: true},
		{name: "DSCF0001.RAF", base: "DSCF0001", ext: "RAF", ok: true},
		{name: "DSC01764.ARW", base: "DSC01764", ext: "ARW", ok: true}, // Sony
		{name: "_DSC0001.ARW", base: "_DSC0001", ext: "ARW", ok: true}, // Sony AdobeRGB
		{name: "C0001.MP4", base: "C0001", ext: "MP4", ok: true},       // Sony video
		{name: "IMG_0001.JPG", base: "IMG_0001", ext: "JPG", ok: true},
		{name: "DSCF0001.MOV", base: "DSCF0001", ext: "MOV", ok: true},
		// lower case on disk still parses, and the ext normalizes up
		{name: "dscf0001.jpg", base: "dscf0001", ext: "JPG", ok: true},
		{name: "notes.txt", ok: false},
		{name: "DSCF0001.XMP", ok: false},
		{name: "DSCF0001", ok: false},
		{name: "leading DSCF0001.JPG", ok: false},
	}
	for _, c := range cases {
		base, ext, ok := SplitMedia(c.name)
		if ok != c.ok || (c.ok && (base != c.base || ext != c.ext)) {
			t.Errorf("SplitMedia(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.name, base, ext, ok, c.base, c.ext, c.ok)
		}
	}
}

// An `ls` line carries an object ID, a size and a date beside the name. None
// of those columns may be mistaken for a media folder or file — which is why
// matching happens on parsed entries and every pattern is anchored.
func TestListingColumnsAreNotMedia(t *testing.T) {
	columns := []string{"2648", "268435457", "ExifJpeg", "14379874", "2026-05-10", "15:02:50"}
	for _, c := range columns {
		if c == "2026-05-10" {
			continue // a bare date IS a valid Sony folder name; context decides
		}
		if IsMediaFolder(c) {
			t.Errorf("IsMediaFolder(%q) = true, want false", c)
		}
		if _, _, ok := SplitMedia(c); ok {
			t.Errorf("SplitMedia(%q) ok = true, want false", c)
		}
	}
}

func TestRawExtAndDisplayExt(t *testing.T) {
	cases := []struct {
		files        map[string]string
		raw, display string
	}{
		{files: map[string]string{"JPG": "DSCF0001.JPG", "RAF": "DSCF0001.RAF"}, raw: "RAF", display: "JPG"},
		{files: map[string]string{"ARW": "DSC01764.ARW"}, raw: "ARW", display: "ARW"},
		{files: map[string]string{"JPG": "DSC01764.JPG", "ARW": "DSC01764.ARW"}, raw: "ARW", display: "JPG"},
		{files: map[string]string{"MP4": "C0001.MP4"}, raw: "", display: "MP4"},
	}
	for _, c := range cases {
		s := &Shot{Files: c.files}
		if got := s.RawExt(); got != c.raw {
			t.Errorf("RawExt(%v) = %q, want %q", c.files, got, c.raw)
		}
		if got := s.DisplayExt(); got != c.display {
			t.Errorf("DisplayExt(%v) = %q, want %q", c.files, got, c.display)
		}
	}
}

func TestMediaFolderPrefix(t *testing.T) {
	cases := []struct{ dir, want string }{
		{"SLOT 1/DCIM/151_FUJI", "SLOT 1/DCIM"},
		{"DCIM/100MSDCF", "DCIM"},
		{"2026-08-12", ""}, // Sony MTP: media folder sits at the storage root
		{"whatever/else", "whatever/else"},
	}
	for _, c := range cases {
		if got := MediaFolderPrefix(c.dir); got != c.want {
			t.Errorf("MediaFolderPrefix(%q) = %q, want %q", c.dir, got, c.want)
		}
	}
}
