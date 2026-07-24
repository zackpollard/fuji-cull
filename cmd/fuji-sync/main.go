// Command fuji-sync is the self-hosted relay that syncs fuji-cull culling
// progress across devices. It is a thin, stateless-per-request HTTP service over
// a mutex-guarded JSON store: clients push their dirty decisions and pull deltas
// since their high-water mark. Every device keeps a full local copy, so the
// server is only a relay — never the sole copy of any decision.
//
//	fuji-sync --listen :8777 --db /data/sync.db --api-key <secret>
//	SYNC_API_KEY=<secret> fuji-sync   (env alternative for the key)
package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/zack/fuji-tools/internal/synccore"
)

func main() {
	listen := flag.String("listen", envOr("SYNC_LISTEN", ":8777"), "listen address")
	db := flag.String("db", envOr("SYNC_DB", "/data/sync.db"), "path to the JSON store")
	apiKey := flag.String("api-key", os.Getenv("SYNC_API_KEY"), "shared secret (x-api-key); also SYNC_API_KEY")
	healthcheck := flag.Bool("healthcheck", false, "probe the local /api/sync/health endpoint and exit (for container healthchecks)")
	flag.Parse()

	if *healthcheck {
		os.Exit(runHealthcheck(*listen))
	}

	if *apiKey == "" {
		log.Fatal("fuji-sync: no API key set (--api-key or SYNC_API_KEY) — refusing to run open")
	}
	st, err := openStore(*db)
	if err != nil {
		log.Fatalf("fuji-sync: open store %s: %v", *db, err)
	}
	log.Printf("fuji-sync: store %s ready (epoch %s)", *db, st.data.Epoch)

	srv := &server{store: st, apiKey: *apiKey}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/sync/health", srv.health)
	mux.HandleFunc("POST /api/sync/push", srv.auth(srv.push))
	mux.HandleFunc("GET /api/sync/pull", srv.auth(srv.pull))

	log.Printf("fuji-sync: listening on %s", *listen)
	if err := http.ListenAndServe(*listen, mux); err != nil {
		log.Fatalf("fuji-sync: %v", err)
	}
}

type server struct {
	store  *store
	apiKey string
}

// auth wraps a handler with a constant-time x-api-key check.
func (s *server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("x-api-key")
		if subtle.ConstantTimeCompare([]byte(key), []byte(s.apiKey)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"ok": true})
}

func (s *server) push(w http.ResponseWriter, r *http.Request) {
	var req synccore.PushRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Camera == "" {
		http.Error(w, "camera required", http.StatusBadRequest)
		return
	}
	resp := s.store.push(req, time.Now().UnixMilli())
	writeJSON(w, resp)
}

func (s *server) pull(w http.ResponseWriter, r *http.Request) {
	camera := r.URL.Query().Get("camera")
	if camera == "" {
		http.Error(w, "camera required", http.StatusBadRequest)
		return
	}
	since := int64(0)
	if v := r.URL.Query().Get("since"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			http.Error(w, "bad since", http.StatusBadRequest)
			return
		}
		since = n
	}
	resp := s.store.pull(camera, since, time.Now().UnixMilli())
	writeJSON(w, resp)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// runHealthcheck probes the health endpoint on the configured listen address and
// returns a process exit code, so the distroless image (no curl) can self-check.
func runHealthcheck(listen string) int {
	host := listen
	if len(host) > 0 && host[0] == ':' {
		host = "127.0.0.1" + host
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + host + "/api/sync/health")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
