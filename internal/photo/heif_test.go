package photo

import "testing"

// The camera names the file .HIF and `ls` reports that; the bulk lsprops-all
// listing calls the same object .HEIC. Discovery uses whichever listing the
// camera answers, so both spellings must be recognised or the shots appear on
// one code path and vanish on the other.
func TestHEIFBothSpellings(t *testing.T) {
	for _, name := range []string{"DSCF6064.HIF", "DSCF6064.HEIC", "dscf6064.hif"} {
		base, ext, ok := SplitMedia(name)
		if !ok {
			t.Errorf("SplitMedia(%q) not recognised as media", name)
			continue
		}
		if !IsHEIF(ext) {
			t.Errorf("SplitMedia(%q) ext %q not recognised as HEIF", name, ext)
		}
		if base == "" {
			t.Errorf("SplitMedia(%q) gave empty base", name)
		}
	}
	if IsHEIF("JPG") || IsHEIF("RAF") {
		t.Error("non-HEIF extension classified as HEIF")
	}
}

func TestDisplayExtPrefersShowableThenHEIF(t *testing.T) {
	cases := []struct {
		files map[string]string
		want  string
	}{
		{map[string]string{"JPG": "a.JPG", "HIF": "a.HIF"}, "JPG"},
		{map[string]string{"HIF": "a.HIF"}, "HIF"},
		{map[string]string{"HEIC": "a.HEIC"}, "HEIC"},
		// RAW+HEIF: the HEIF is the camera's own rendering, so it wins
		{map[string]string{"RAF": "a.RAF", "HIF": "a.HIF"}, "HIF"},
		{map[string]string{"RAF": "a.RAF"}, "RAF"},
	}
	for _, c := range cases {
		if got := (&Shot{Files: c.files}).DisplayExt(); got != c.want {
			t.Errorf("DisplayExt(%v) = %q, want %q", c.files, got, c.want)
		}
	}
}
