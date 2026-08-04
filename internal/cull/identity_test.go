package cull

import "testing"

// The camera lists times as "20260714T151530"; EXIF uses "2026:07:14 15:15:30".
// They must be comparable or the identity check silently never fires.
func TestNormalizePTPTimeMatchesExifLayout(t *testing.T) {
	got := normalizePTPTime("20260714T151530")
	if got != "2026:07:14 15:15:30" {
		t.Fatalf("normalizePTPTime = %q, want EXIF layout", got)
	}
	if captureUnix(got) == 0 {
		t.Fatal("normalized camera time does not parse")
	}
	if captureUnix(got) != captureUnix("2026:07:14 15:15:30") {
		t.Error("camera time and EXIF time do not compare equal")
	}
}

// Absence must never reject a good file: videos have no DateTimeOriginal, and
// listings from an older cache carry no timestamp at all.
func TestNormalizePTPTimeLeavesUnknownAlone(t *testing.T) {
	for _, in := range []string{"", "-", "2026-07-14 15:15:30", "garbage"} {
		if got := normalizePTPTime(in); got != in {
			t.Errorf("normalizePTPTime(%q) = %q, want it untouched", in, got)
		}
	}
	if captureUnix(normalizePTPTime("")) != 0 {
		t.Error("empty timestamp parsed as a real time")
	}
}
