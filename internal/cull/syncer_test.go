package cull

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/zack/fuji-tools/internal/synccore"
)

// fakeSyncServer is a minimal in-test stand-in for cmd/fuji-sync that reuses the
// exact same merge rule (synccore.Wins), so this test exercises real
// convergence, not a mock that always agrees.
type fakeSyncServer struct {
	mu      sync.Mutex
	epoch   string
	version int64
	rows    map[string]synccore.DecisionRow // ckey -> stored (with Version)
}

func (f *fakeSyncServer) has(ckey string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.rows[ckey]
	return ok
}

func newFakeSyncServer() (*httptest.Server, *fakeSyncServer) {
	f := &fakeSyncServer{epoch: "e1", rows: map[string]synccore.DecisionRow{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/sync/push", func(w http.ResponseWriter, r *http.Request) {
		var req synccore.PushRequest
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		defer f.mu.Unlock()
		resp := synccore.PushResponse{Epoch: f.epoch, ServerNow: time.Now().UnixMilli()}
		for _, in := range req.Decisions {
			st, ok := f.rows[in.Ckey]
			if synccore.Wins(in.HLC, in.Migrated, st.HLC, st.Migrated, ok) {
				f.version++
				in.Version = f.version
				f.rows[in.Ckey] = in
				resp.Results = append(resp.Results, synccore.AckRow{Ckey: in.Ckey, Accepted: true, Version: in.Version, Winner: in})
			} else {
				resp.Results = append(resp.Results, synccore.AckRow{Ckey: in.Ckey, Accepted: false, Version: st.Version, Winner: st})
			}
		}
		resp.CameraVersion = f.version
		json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("/api/sync/pull", func(w http.ResponseWriter, r *http.Request) {
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		f.mu.Lock()
		defer f.mu.Unlock()
		resp := synccore.PullResponse{Epoch: f.epoch, ServerNow: time.Now().UnixMilli(), DeltaHigh: since}
		for _, row := range f.rows {
			if row.Version > since {
				resp.Decisions = append(resp.Decisions, row)
				if row.Version > resp.DeltaHigh {
					resp.DeltaHigh = row.Version
				}
			}
		}
		json.NewEncoder(w).Encode(resp)
	})
	return httptest.NewServer(mux), f
}

// testKeys mirrors a single card whose shots' canonical keys equal their legacy
// IDs, so the resolver is an identity map covering the frames used in the tests.
var testKeys = []string{"151_FUJI/DSCF0001", "151_FUJI/DSCF0002", "151_FUJI/DSCF0003"}

func identityResolvers() (map[string]string, map[string][]string) {
	canon := map[string]string{}
	leg := map[string][]string{}
	for _, k := range testKeys {
		canon[k] = k
		leg[k] = []string{k}
	}
	return canon, leg
}

func newSyncedSession(t *testing.T, name string) *Session {
	t.Helper()
	s, err := loadSession(filepath.Join(t.TempDir(), name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	s.SetResolvers(identityResolvers())
	return s
}

// The headline test: a decision made on device A appears on device B after each
// runs a sync cycle against a shared server — proving the goal end to end.
func TestTwoDevicesConverge(t *testing.T) {
	srv, _ := newFakeSyncServer()
	defer srv.Close()
	client := newSyncClient(srv.URL, "k")

	a := newSyncedSession(t, "A")
	b := newSyncedSession(t, "B")

	// A keeps a shot, then syncs up
	a.SetDecision("151_FUJI/DSCF0001", "keep")
	if err := syncOnce(client, a, "cam"); err != nil {
		t.Fatal(err)
	}
	// B has nothing yet; a sync pulls A's decision
	if err := syncOnce(client, b, "cam"); err != nil {
		t.Fatal(err)
	}
	if got := b.Decisions()["151_FUJI/DSCF0001"]; got != "keep" {
		t.Fatalf("B did not receive A's decision: %q", got)
	}

	// B changes its mind to reject; A picks it up
	b.SetDecision("151_FUJI/DSCF0001", "reject")
	syncOnce(client, b, "cam")
	syncOnce(client, a, "cam")
	if got := a.Decisions()["151_FUJI/DSCF0001"]; got != "reject" {
		t.Fatalf("A did not converge to B's newer decision: %q", got)
	}

	// A clears it (tombstone); B ends up with it cleared, not resurrected
	a.SetDecision("151_FUJI/DSCF0001", "")
	syncOnce(client, a, "cam")
	syncOnce(client, b, "cam")
	if _, present := b.Decisions()["151_FUJI/DSCF0001"]; present {
		t.Fatalf("B should see the cleared decision, still has %v", b.Decisions())
	}
}

// Offline divergence: both edit different shots offline, then both sync — no loss.
func TestOfflineDivergenceMerges(t *testing.T) {
	srv, _ := newFakeSyncServer()
	defer srv.Close()
	client := newSyncClient(srv.URL, "k")
	a := newSyncedSession(t, "A")
	b := newSyncedSession(t, "B")

	a.SetDecision("151_FUJI/DSCF0001", "keep")
	a.SetDecision("151_FUJI/DSCF0002", "reject")
	b.SetDecision("151_FUJI/DSCF0003", "keep")

	// each syncs; run twice so both directions settle
	for i := 0; i < 2; i++ {
		syncOnce(client, a, "cam")
		syncOnce(client, b, "cam")
		syncOnce(client, a, "cam")
	}
	for _, s := range []*Session{a, b} {
		d := s.Decisions()
		if d["151_FUJI/DSCF0001"] != "keep" || d["151_FUJI/DSCF0002"] != "reject" || d["151_FUJI/DSCF0003"] != "keep" {
			t.Errorf("device did not converge to the union: %v", d)
		}
	}
}

// A migrated decision (from a v1 upgrade) seeds the server, but a genuine remote
// edit still wins — the migration can't bulldoze real cross-device work.
func TestMigratedSeedLosesToGenuine(t *testing.T) {
	srv, _ := newFakeSyncServer()
	defer srv.Close()
	client := newSyncClient(srv.URL, "k")

	// A upgrades from v1 with a keep (migrated)
	pa := filepath.Join(t.TempDir(), "A.json")
	writeV1(t, pa, map[string]string{"151_FUJI/DSCF0001": "keep"}, time.Now())
	a, _ := loadSession(pa)
	a.SetResolvers(identityResolvers())

	// B makes a genuine reject and syncs first
	b := newSyncedSession(t, "B")
	b.SetDecision("151_FUJI/DSCF0001", "reject")
	syncOnce(client, b, "cam")

	// A syncs: its migrated keep loses to B's genuine reject
	syncOnce(client, a, "cam")
	if got := a.Decisions()["151_FUJI/DSCF0001"]; got != "reject" {
		t.Fatalf("migrated seed should lose to genuine edit, A has %q", got)
	}
}

// The runtime wiring: a decision nudges the running syncer loop, which pushes it
// to the server without any manual syncOnce call.
func TestSyncerLoopPushesOnNudge(t *testing.T) {
	srv, fake := newFakeSyncServer()
	defer srv.Close()
	client := newSyncClient(srv.URL, "k")
	a := newSyncedSession(t, "A")

	sy := newSyncer(client, func() (*Session, string) { return a, "cam" })
	a.SetOnDirty(sy.Nudge)
	go sy.Run()
	defer sy.Stop()

	a.SetDecision("151_FUJI/DSCF0001", "keep") // fires Nudge -> loop pushes

	deadline := time.Now().Add(5 * time.Second)
	for !fake.has("151_FUJI/DSCF0001") {
		if time.Now().After(deadline) {
			t.Fatal("syncer loop did not push the decision after a nudge")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// and the local record is now marked clean (acked)
	deadline = time.Now().Add(2 * time.Second)
	for len(a.Outbox()) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("record not acked after push, outbox=%v", a.Outbox())
		}
		time.Sleep(20 * time.Millisecond)
	}
}
