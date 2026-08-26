package cull

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// stubTransport is a camera link that refuses PTP and serves a ready object
// catalog — the exact shape that used to make the fallback win a race it should
// have lost.
type stubTransport struct {
	ptpErr    error
	ptpCalls  int
	objCalls  int
	catalogOK bool
}

func (s *stubTransport) SendPTP(command, outData []byte) ([]byte, error) {
	s.ptpCalls++
	return nil, s.ptpErr
}

func (s *stubTransport) Folders() ([]byte, error) {
	s.objCalls++
	if !s.catalogOK {
		return nil, errors.New("camera index not ready")
	}
	return []byte(`[{"dir":"SLOT 1/DCIM/151_FUJI","folder":"151_FUJI"}]`), nil
}

func (s *stubTransport) Entries(dir string) ([]byte, error) {
	return []byte(`[{"objectID":"SLOT 1/DCIM/151_FUJI/DSCF0001.JPG","name":"DSCF0001.JPG","size":100,"date":"2026-07-06"}]`), nil
}

func (s *stubTransport) ReadAt(objectID string, offset, size int64) ([]byte, error) {
	return nil, errors.New("not used")
}
func (s *stubTransport) Download(objectID, destPath string) error { return nil }
func (s *stubTransport) Connected() bool                          { return true }

// The fallback is permanent for the session — the two paths use different
// object IDs and nothing rebuilds the catalog — so which one a session lands on
// has to be visible rather than inferred from log archaeology.
func TestFallbackIsReportedAsTheActiveIndexPath(t *testing.T) {
	st := &stubTransport{ptpErr: errors.New("PTP GetDeviceInfo timed out after 30s"), catalogOK: true}
	b := &iccBackend{t: st}
	nop := func(string, int) {}

	if got := b.IndexPath(); got != IndexNone {
		t.Errorf("IndexPath = %q before any success, want empty", got)
	}
	out, err := b.Discover(context.Background(), nop)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d listings, want 1", len(out))
	}
	if got := b.IndexPath(); got != IndexICCObjects {
		t.Errorf("IndexPath = %q, want %q", got, IndexICCObjects)
	}
}

// A camera whose catalog is not ready either must report both failures, so the
// log says which of the two paths is the problem rather than only the last one.
func TestBothPathsUnavailableReportsBoth(t *testing.T) {
	st := &stubTransport{ptpErr: errors.New("PTP refused: DeviceBusy (0x2019)"), catalogOK: false}
	b := &iccBackend{t: st}

	_, err := b.Discover(context.Background(), func(string, int) {})
	if err == nil {
		t.Fatal("expected an error when neither path works")
	}
	if !strings.Contains(err.Error(), "DeviceBusy") {
		t.Errorf("error %v drops the PTP reason", err)
	}
	if !strings.Contains(err.Error(), "not ready") {
		t.Errorf("error %v drops the catalog reason", err)
	}
}
