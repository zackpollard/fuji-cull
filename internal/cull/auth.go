package cull

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
)

// Engine auth for network exposure (the "remote camera host" mode). When
// App.engineKey is empty the engine is unauthenticated — its historical
// loopback behavior, so nothing changes for a device running its own embedded
// engine. When set, every /api/* call must present the key, letting the engine
// be safely bound to 0.0.0.0 so other devices on the LAN can browse the one
// host that has the camera plugged in.
//
// The key may arrive three ways so every client works:
//   - x-api-key header  — native apps (URLSession / OkHttp control the request)
//   - ?key=<key> query  — media URLs handed to players (mpv/ExoPlayer) or <img>/<video>
//   - fujicull_auth cookie — a browser, set once via POST /api/auth so <img>/<video>
//     tags (which can't send headers) authenticate automatically
const authCookie = "fujicull_auth"

// gatedPath reports whether a path requires the engine key. Static assets (the
// web app shell, fonts) stay open so the browser can load the login prompt;
// /api/auth and /api/health are the unauthenticated handshake/liveness points.
func gatedPath(p string) bool {
	return strings.HasPrefix(p, "/api/") && p != "/api/auth" && p != "/api/health"
}

// keyOK constant-time-compares a presented key against the engine key.
func (a *App) keyOK(k string) bool {
	return k != "" && subtle.ConstantTimeCompare([]byte(k), []byte(a.engineKey)) == 1
}

// authed reports whether the request carries a valid key (header, query, or cookie).
func (a *App) authed(r *http.Request) bool {
	if a.keyOK(r.Header.Get("x-api-key")) {
		return true
	}
	if a.keyOK(r.URL.Query().Get("key")) {
		return true
	}
	if c, err := r.Cookie(authCookie); err == nil && a.keyOK(c.Value) {
		return true
	}
	return false
}

// withAuth wraps the handler chain; a no-op when no engine key is configured.
func (a *App) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.engineKey != "" && gatedPath(r.URL.Path) && !a.authed(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="fuji-cull"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// registerAuthRoutes adds the unauthenticated handshake + liveness endpoints.
func (a *App) registerAuthRoutes(mux *http.ServeMux) {
	// POST /api/auth {key} — validate and set the browser cookie so subsequent
	// <img>/<video> loads authenticate. Native apps use the header and don't
	// need this.
	mux.HandleFunc("POST /api/auth", func(w http.ResponseWriter, r *http.Request) {
		if a.engineKey == "" {
			writeJSON(w, map[string]any{"ok": true, "required": false})
			return
		}
		var body struct {
			Key string `json:"key"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		key := body.Key
		if key == "" {
			key = r.URL.Query().Get("key")
		}
		if !a.keyOK(key) {
			http.Error(w, "invalid key", http.StatusUnauthorized)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     authCookie,
			Value:    key,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   30 * 24 * 3600,
		})
		writeJSON(w, map[string]any{"ok": true, "required": true})
	})

	// GET /api/health — liveness + whether a key is required + readiness, so a
	// client can verify the URL is a fuji-cull engine before entering the key.
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"ok":           true,
			"service":      "fuji-cull",
			"ready":        a.isReady(),
			"authRequired": a.engineKey != "",
		})
	})
}
