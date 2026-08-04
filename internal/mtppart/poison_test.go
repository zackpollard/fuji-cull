package mtppart

import (
	"bufio"
	"io"
	"strings"
	"testing"
)

// A response carries no reference to its request, so a half-read reply leaves
// the pipe pointing into binary payload. The next read would find a 0x01 byte
// inside those bytes and return a slice of a DIFFERENT object as though it
// were the one requested — wrong data, no error. The session must refuse
// instead.
func TestSessionRefusesReuseAfterFramingLoss(t *testing.T) {
	// A reply that promises 64 bytes but delivers 8, then more traffic that
	// happens to contain the marker byte and a plausible header.
	stream := "\x01OK 64\n" + strings.Repeat("A", 8) +
		"\x01OK 5\nWRONG"
	s := &Server{
		in:  nopWriteCloser{io.Discard},
		out: bufio.NewReader(strings.NewReader(stream)),
	}

	if _, err := s.ReadAt("111", 0, 64); err == nil {
		t.Fatal("a truncated payload was accepted as a complete read")
	}
	// The dangerous part is what happens NEXT.
	got, err := s.ReadAt("222", 0, 5)
	if err == nil {
		t.Fatalf("poisoned session served %q for a later request — this is the silent wrong-object failure", got)
	}
	if !strings.Contains(err.Error(), "poisoned") {
		t.Errorf("expected a poisoned-session error, got %v", err)
	}
}

// A framed ERR reply consumes nothing extra, so the session stays usable.
func TestErrReplyDoesNotPoison(t *testing.T) {
	stream := "\x01ERR no such object\n" + "\x01OK 3\nabc"
	s := &Server{
		in:  nopWriteCloser{io.Discard},
		out: bufio.NewReader(strings.NewReader(stream)),
	}
	if _, err := s.ReadAt("111", 0, 10); err == nil {
		t.Fatal("ERR reply reported as success")
	}
	got, err := s.ReadAt("222", 0, 3)
	if err != nil {
		t.Fatalf("session was discarded after a recoverable ERR: %v", err)
	}
	if string(got) != "abc" {
		t.Errorf("got %q, want abc", got)
	}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
