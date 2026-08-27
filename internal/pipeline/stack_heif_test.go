package pipeline

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/zack/fuji-tools/internal/immich"
	"github.com/zack/fuji-tools/internal/photo"
)

// stackSrv answers the pre-flight "what is already stacked" query with the
// given asset ids and records every stack that gets created.
func stackSrv(t *testing.T, already []string) (*httptest.Server, *[][]string) {
	t.Helper()
	var mu sync.Mutex
	var created [][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			assets := make([]map[string]string, 0, len(already))
			for _, id := range already {
				assets = append(assets, map[string]string{"id": id})
			}
			json.NewEncoder(w).Encode([]map[string]any{{"primaryAssetId": "", "assets": assets}})
			return
		}
		var body struct {
			AssetIDs []string `json:"assetIds"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		created = append(created, body.AssetIDs)
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)
	return srv, &created
}

// A card shot in HEIF+RAW produces HEIC+RAF pairs. Matching only JPG meant
// every one was passed over in silence — the stage reported nothing to do and
// looked like it had worked.
func TestStackPairsStacksHEIFWithRaw(t *testing.T) {
	srv, created := stackSrv(t, nil)
	files := []photo.FileEntry{
		{Folder: "156_FUJI", Name: "DSCF8581.HEIC", AssetID: "heic1"},
		{Folder: "156_FUJI", Name: "DSCF8581.RAF", AssetID: "raf1"},
		{Folder: "156_FUJI", Name: "DSCF8582.HIF", AssetID: "hif2"},
		{Folder: "156_FUJI", Name: "DSCF8582.RAF", AssetID: "raf2"},
	}
	StackPairs(context.Background(), Options{}, immich.NewClient(srv.URL, "k"), files)

	if len(*created) != 2 {
		t.Fatalf("created %d stacks, want 2 (one per HEIF+RAF pair): %v", len(*created), *created)
	}
	for _, st := range *created {
		if len(st) != 2 {
			t.Fatalf("stack %v does not hold exactly a rendition and a raw", st)
		}
		// The rendition leads: it is the frame the camera itself produced.
		if st[0] != "heic1" && st[0] != "hif2" {
			t.Errorf("stack %v does not lead with the HEIF rendition", st)
		}
	}
}

// The whole point of the re-run: nothing was uploaded this time, every asset
// id came back from the server as an existing one, and the pairs must still
// get stacked.
func TestStackPairsWorksOnAnAlreadyUploadedCard(t *testing.T) {
	srv, created := stackSrv(t, nil)
	files := []photo.FileEntry{
		{Folder: "156_FUJI", Name: "DSCF0001.HEIC", AssetID: "dup-heic"},
		{Folder: "156_FUJI", Name: "DSCF0001.RAF", AssetID: "dup-raf"},
	}
	StackPairs(context.Background(), Options{}, immich.NewClient(srv.URL, "k"), files)
	if len(*created) != 1 {
		t.Fatalf("created %d stacks, want 1 — a duplicate-only run must still stack", len(*created))
	}
}

// A pair the server already has stacked is left alone, so running the import
// again is a no-op rather than a second attempt per pair.
func TestStackPairsSkipsWhatIsAlreadyStacked(t *testing.T) {
	srv, created := stackSrv(t, []string{"heic1", "raf1"})
	files := []photo.FileEntry{
		{Folder: "156_FUJI", Name: "DSCF0001.HEIC", AssetID: "heic1"},
		{Folder: "156_FUJI", Name: "DSCF0001.RAF", AssetID: "raf1"},
		{Folder: "156_FUJI", Name: "DSCF0002.HEIC", AssetID: "heic2"},
		{Folder: "156_FUJI", Name: "DSCF0002.RAF", AssetID: "raf2"},
	}
	StackPairs(context.Background(), Options{}, immich.NewClient(srv.URL, "k"), files)

	if len(*created) != 1 {
		t.Fatalf("created %d stacks, want only the unstacked pair: %v", len(*created), *created)
	}
	if (*created)[0][0] != "heic2" {
		t.Errorf("stacked %v, want the pair that was not already stacked", (*created)[0])
	}
}

// A key without stack.read cannot tell what is stacked. Guessing "everything
// is stacked" would silently stop stacking altogether, so the run goes ahead
// and lets the server rule on each pair.
func TestStackPairsProceedsWhenItCannotCheck(t *testing.T) {
	var mu sync.Mutex
	var created int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"message":"Missing required permission: stack.read"}`))
			return
		}
		mu.Lock()
		created++
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	StackPairs(context.Background(), Options{}, immich.NewClient(srv.URL, "k"), []photo.FileEntry{
		{Folder: "156_FUJI", Name: "DSCF0001.HEIC", AssetID: "h1"},
		{Folder: "156_FUJI", Name: "DSCF0001.RAF", AssetID: "r1"},
	})
	if created != 1 {
		t.Errorf("created %d stacks, want 1 — an unreadable stack list must not stop stacking", created)
	}
}
