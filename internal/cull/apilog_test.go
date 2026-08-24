package cull

import (
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Failures must reach the log with the message the client was given. Thirty-odd
// handlers call http.Error and none of them logged, while every client discards
// the body — so an engine error was invisible at both ends at once.
func TestLogFailuresRecordsStatusAndMessage(t *testing.T) {
	cases := []struct {
		name       string
		handler    http.HandlerFunc
		wantLogged bool
		wantParts  []string
	}{
		{
			name: "camera failure carries its message",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "aft-mtp-cli failed: stale buffer", http.StatusBadGateway)
			},
			wantLogged: true,
			wantParts:  []string{"502", "/api/thing", "stale buffer"},
		},
		{
			name: "a refused import is worth seeing too",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "an import is already running", http.StatusConflict)
			},
			wantLogged: true,
			wantParts:  []string{"409", "already running"},
		},
		{
			name: "404 is routine here, not a fault",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "no thumbnail", http.StatusNotFound)
			},
			wantLogged: false,
		},
		{
			name:       "success is not noise",
			handler:    func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) },
			wantLogged: false,
		},
		{
			name: "a bare status with no body still logs",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			wantLogged: true,
			wantParts:  []string{"500", "/api/thing"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf strings.Builder
			old := log.Writer()
			log.SetOutput(&buf)
			defer log.SetOutput(old)

			rec := httptest.NewRecorder()
			logFailures(tc.handler).ServeHTTP(rec, httptest.NewRequest("GET", "/api/thing", nil))

			got := buf.String()
			if logged := strings.Contains(got, "api:"); logged != tc.wantLogged {
				t.Fatalf("logged=%v want %v (log: %q)", logged, tc.wantLogged, got)
			}
			for _, p := range tc.wantParts {
				if !strings.Contains(got, p) {
					t.Errorf("log %q missing %q", got, p)
				}
			}
		})
	}
}

// The body copy is bounded: a handler that writes a huge error must not put all
// of it in the log, and must still write all of it to the client.
func TestLogFailuresBoundsTheLoggedBody(t *testing.T) {
	var buf strings.Builder
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	big := strings.Repeat("x", 5000)
	rec := httptest.NewRecorder()
	logFailures(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, big, http.StatusInternalServerError)
	})).ServeHTTP(rec, httptest.NewRequest("GET", "/api/thing", nil))

	if n := strings.Count(buf.String(), "x"); n > maxLoggedBody {
		t.Errorf("logged %d body bytes, cap is %d", n, maxLoggedBody)
	}
	if n := strings.Count(rec.Body.String(), "x"); n != len(big) {
		t.Errorf("client got %d bytes of the message, want %d", n, len(big))
	}
}
