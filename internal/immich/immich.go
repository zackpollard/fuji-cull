// Package immich is a minimal client for the Immich upload/validation API.
package immich

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/zack/fuji-tools/internal/photo"
)

type Client struct {
	URL    string
	APIKey string
	HTTP   *http.Client
}

func NewClient(url, key string) *Client {
	return &Client{
		URL:    url,
		APIKey: key,
		HTTP: &http.Client{
			Timeout: 30 * time.Minute, // accommodate large file uploads
		},
	}
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	req.Header.Set("x-api-key", c.APIKey)
	req.Header.Set("Accept", "application/json")
	return c.HTTP.Do(req)
}

// uploadResponse matches Immich's /api/asset/upload response.
type uploadResponse struct {
	ID        string `json:"id"`
	Status    string `json:"status"`    // newer API: "created" | "duplicate"
	Duplicate bool   `json:"duplicate"` // older API field
}

// Upload sends one asset to Immich using streaming multipart (memory-safe for multi-GB files).
// Returns assetID, duplicate flag.
func (c *Client) Upload(ctx context.Context, f *photo.FileEntry) (string, bool, error) {
	file, err := os.Open(f.LocalPath)
	if err != nil {
		return "", false, fmt.Errorf("open %s: %w", f.LocalPath, err)
	}
	defer file.Close()
	st, err := file.Stat()
	if err != nil {
		return "", false, err
	}
	mtime := st.ModTime().UTC().Format(time.RFC3339)

	devAssetID := f.SHA1
	if devAssetID == "" {
		devAssetID = f.Folder + "/" + f.Name
	}

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	contentType := mw.FormDataContentType()

	go func() {
		// Whatever happens, close so the reader unblocks.
		defer func() {
			_ = mw.Close()
			_ = pw.Close()
		}()
		write := func(name, val string) error { return mw.WriteField(name, val) }
		for _, kv := range []struct{ k, v string }{
			{"deviceAssetId", devAssetID},
			{"deviceId", "fuji-import"},
			{"fileCreatedAt", mtime},
			{"fileModifiedAt", mtime},
			{"isFavorite", "false"},
		} {
			if err := write(kv.k, kv.v); err != nil {
				pw.CloseWithError(err)
				return
			}
		}
		part, err := mw.CreateFormFile("assetData", filepath.Base(f.LocalPath))
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		if _, err := io.Copy(part, file); err != nil {
			pw.CloseWithError(err)
			return
		}
	}()

	req, err := http.NewRequestWithContext(ctx, "POST", c.URL+"/api/asset/upload", pr)
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Content-Type", contentType)
	req.ContentLength = -1 // unknown; transferred chunked

	resp, err := c.do(req)
	if err != nil {
		return "", false, fmt.Errorf("upload %s: %w", f.Name, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", false, fmt.Errorf("upload %s: HTTP %d: %s",
			f.Name, resp.StatusCode, truncate(string(respBody), 400))
	}
	var ur uploadResponse
	if err := json.Unmarshal(respBody, &ur); err != nil {
		return "", false, fmt.Errorf("decode upload response: %w; body=%s",
			err, truncate(string(respBody), 400))
	}
	duplicate := ur.Duplicate || ur.Status == "duplicate"
	return ur.ID, duplicate, nil
}

// BulkCheckResult matches Immich's bulk-upload-check API request/response shape.
type BulkCheckResult struct {
	Action  string `json:"action"`  // "accept" | "reject"
	Reason  string `json:"reason"`  // "duplicate" when reject
	AssetID string `json:"assetId"` // present when duplicate
	ID      string `json:"id"`      // echoed request id
}

type bulkCheckRequest struct {
	Assets []struct {
		ID       string `json:"id"`
		Checksum string `json:"checksum"`
	} `json:"assets"`
}

type bulkCheckResponse struct {
	Results []BulkCheckResult `json:"results"`
}

func (c *Client) BulkCheck(ctx context.Context, ids, checksumsB64 []string) (map[string]BulkCheckResult, error) {
	if len(ids) != len(checksumsB64) {
		return nil, fmt.Errorf("BulkCheck: ids/checksums length mismatch (%d vs %d)", len(ids), len(checksumsB64))
	}
	var req bulkCheckRequest
	for i := range ids {
		req.Assets = append(req.Assets, struct {
			ID       string `json:"id"`
			Checksum string `json:"checksum"`
		}{ID: ids[i], Checksum: checksumsB64[i]})
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		c.URL+"/api/asset/bulk-upload-check", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("bulk-upload-check: HTTP %d: %s",
			resp.StatusCode, truncate(string(respBody), 400))
	}
	var br bulkCheckResponse
	if err := json.Unmarshal(respBody, &br); err != nil {
		return nil, fmt.Errorf("decode bulk-check response: %w; body=%s",
			err, truncate(string(respBody), 400))
	}
	out := make(map[string]BulkCheckResult, len(br.Results))
	for _, r := range br.Results {
		out[r.ID] = r
	}
	return out, nil
}

// EnsureAlbum returns the id of an album with the given name, creating it if absent.
func (c *Client) EnsureAlbum(ctx context.Context, name string) (string, error) {
	// First search by name.
	req, err := http.NewRequestWithContext(ctx, "GET", c.URL+"/api/album", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.do(req)
	if err != nil {
		return "", err
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("list albums: HTTP %d: %s",
			resp.StatusCode, truncate(string(respBody), 400))
	}
	var albums []struct {
		ID        string `json:"id"`
		AlbumName string `json:"albumName"`
	}
	if err := json.Unmarshal(respBody, &albums); err != nil {
		return "", fmt.Errorf("decode albums: %w", err)
	}
	for _, a := range albums {
		if a.AlbumName == name {
			return a.ID, nil
		}
	}
	// Create.
	body, _ := json.Marshal(map[string]any{"albumName": name})
	req, _ = http.NewRequestWithContext(ctx, "POST", c.URL+"/api/album", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err = c.do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ = io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("create album: HTTP %d: %s",
			resp.StatusCode, truncate(string(respBody), 400))
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(respBody, &created); err != nil {
		return "", fmt.Errorf("decode create album: %w", err)
	}
	return created.ID, nil
}

// AddToAlbum adds asset IDs to the given album. Idempotent on Immich's side (returns "duplicate" entries).
func (c *Client) AddToAlbum(ctx context.Context, albumID string, assetIDs []string) error {
	const batch = 200
	for start := 0; start < len(assetIDs); start += batch {
		end := start + batch
		if end > len(assetIDs) {
			end = len(assetIDs)
		}
		body, _ := json.Marshal(map[string]any{"ids": assetIDs[start:end]})
		req, err := http.NewRequestWithContext(ctx, "PUT",
			c.URL+"/api/album/"+albumID+"/assets", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.do(req)
		if err != nil {
			return err
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			return fmt.Errorf("add to album: HTTP %d: %s",
				resp.StatusCode, truncate(string(respBody), 400))
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
