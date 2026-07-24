package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/zack/fuji-tools/internal/synccore"
)

func testServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	st, err := openStore(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &server{store: st, apiKey: "secret"}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/sync/health", srv.health)
	mux.HandleFunc("POST /api/sync/push", srv.auth(srv.push))
	mux.HandleFunc("GET /api/sync/pull", srv.auth(srv.pull))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, "secret"
}

func TestHTTPAuthAndConverge(t *testing.T) {
	ts, key := testServer(t)

	// no key -> 401
	resp, _ := http.Post(ts.URL+"/api/sync/push", "application/json", bytes.NewReader([]byte(`{"camera":"cam"}`)))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("push without key = %d, want 401", resp.StatusCode)
	}

	// device A pushes a keep
	push := func(body synccore.PushRequest) synccore.PushResponse {
		buf, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", ts.URL+"/api/sync/push", bytes.NewReader(buf))
		req.Header.Set("x-api-key", key)
		r, err := http.DefaultClient.Do(req)
		if err != nil || r.StatusCode != 200 {
			t.Fatalf("push failed: %v %v", err, r.StatusCode)
		}
		var out synccore.PushResponse
		json.NewDecoder(r.Body).Decode(&out)
		return out
	}
	pull := func(since int64) synccore.PullResponse {
		req, _ := http.NewRequest("GET", ts.URL+"/api/sync/pull?camera=cam&since="+strconv.FormatInt(since, 10), nil)
		req.Header.Set("x-api-key", key)
		r, _ := http.DefaultClient.Do(req)
		var out synccore.PullResponse
		json.NewDecoder(r.Body).Decode(&out)
		return out
	}

	push(synccore.PushRequest{Camera: "cam", DeviceID: "A", Decisions: []synccore.DecisionRow{
		{Ckey: "151_FUJI/DSCF0001", D: "keep", HLC: synccore.HLC{Wall: 1000, Dev: "A"}},
	}})

	// device B pulls from 0 and sees it
	got := pull(0)
	if len(got.Decisions) != 1 || got.Decisions[0].D != "keep" {
		t.Fatalf("B did not pull A's decision over HTTP: %+v", got.Decisions)
	}

	// B rejects (newer) and pushes; A pulls the delta since the high-water
	push(synccore.PushRequest{Camera: "cam", DeviceID: "B", Decisions: []synccore.DecisionRow{
		{Ckey: "151_FUJI/DSCF0001", D: "reject", HLC: synccore.HLC{Wall: 2000, Dev: "B"}},
	}})
	delta := pull(got.DeltaHigh)
	if len(delta.Decisions) != 1 || delta.Decisions[0].D != "reject" {
		t.Fatalf("delta pull should carry the reject: %+v", delta.Decisions)
	}

	// health is open
	h, _ := http.Get(ts.URL + "/api/sync/health")
	body, _ := io.ReadAll(h.Body)
	if h.StatusCode != 200 || !bytes.Contains(body, []byte(`"ok":true`)) {
		t.Errorf("health = %d %s", h.StatusCode, body)
	}
}
