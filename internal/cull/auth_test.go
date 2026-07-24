package cull

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// authTestHandler builds the auth+readiness chain around a trivial /api probe so
// we can exercise the middleware without a full engine.
func authHandler(key string) http.Handler {
	a := &App{engineKey: key, ready: true}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("shell")) })
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	a.registerAuthRoutes(mux)
	return a.withAuth(mux)
}

func TestAuthOpenWhenNoKey(t *testing.T) {
	h := authHandler("")
	for _, path := range []string{"/", "/api/status", "/api/health"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 {
			t.Errorf("no-key %s = %d, want 200 (open loopback)", path, rec.Code)
		}
	}
}

func TestAuthGatesApiWhenKeySet(t *testing.T) {
	h := authHandler("secret")

	// static shell + health + no key on the gated api
	must := func(path, key, cookie string, want int) {
		t.Helper()
		req := httptest.NewRequest("GET", path, nil)
		if key != "" {
			req.Header.Set("x-api-key", key)
		}
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: authCookie, Value: cookie})
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("GET %s (key=%q cookie=%q) = %d, want %d", path, key, cookie, rec.Code, want)
		}
	}
	must("/", "", "", 200)                 // shell open (loads the login prompt)
	must("/api/health", "", "", 200)       // liveness open
	must("/api/status", "", "", 401)       // gated, no key
	must("/api/status", "secret", "", 200) // header key
	must("/api/status", "wrong", "", 401)  // wrong key
	must("/api/status", "", "secret", 200) // cookie key
	must("/api/status", "", "wrong", 401)  // wrong cookie

	// query-param key (media URLs handed to players / <img>/<video>)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/status?key=secret", nil))
	if rec.Code != 200 {
		t.Errorf("query key = %d, want 200", rec.Code)
	}
}

func TestAuthCookieHandshake(t *testing.T) {
	h := authHandler("secret")
	// POST /api/auth with the right key sets the cookie
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/auth", strings.NewReader(`{"key":"secret"}`)))
	if rec.Code != 200 {
		t.Fatalf("auth handshake = %d, want 200", rec.Code)
	}
	var got *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == authCookie {
			got = c
		}
	}
	if got == nil || got.Value != "secret" || !got.HttpOnly {
		t.Fatalf("auth did not set an HttpOnly cookie: %+v", got)
	}
	// wrong key -> 401, no cookie
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/auth", strings.NewReader(`{"key":"nope"}`)))
	if rec.Code != 401 {
		t.Errorf("bad-key handshake = %d, want 401", rec.Code)
	}
}
