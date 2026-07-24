package cull

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/zack/fuji-tools/internal/synccore"
)

// syncClient talks to the self-hosted fuji-sync server. Stateless per request,
// authenticated with a single x-api-key header — the same shape as the Immich
// client. errAuth is returned on 401 so the syncer can stop hammering a
// misconfigured server.
type syncClient struct {
	base string
	key  string
	http *http.Client
}

var errAuth = fmt.Errorf("sync: unauthorized (check the sync key)")

func newSyncClient(base, key string) *syncClient {
	return &syncClient{
		base: trimTrailingSlash(base),
		key:  key,
		http: &http.Client{Timeout: 60 * time.Second},
	}
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func (c *syncClient) do(method, path string, body any) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, c.base+path, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", c.key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.http.Do(req)
}

func (c *syncClient) push(req synccore.PushRequest) (synccore.PushResponse, error) {
	var out synccore.PushResponse
	resp, err := c.do("POST", "/api/sync/push", req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return out, errAuth
	}
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("sync push: %s", resp.Status)
	}
	return out, json.NewDecoder(resp.Body).Decode(&out)
}

func (c *syncClient) pull(camera string, since int64) (synccore.PullResponse, error) {
	var out synccore.PullResponse
	q := url.Values{}
	q.Set("camera", camera)
	q.Set("since", strconv.FormatInt(since, 10))
	resp, err := c.do("GET", "/api/sync/pull?"+q.Encode(), nil)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return out, errAuth
	}
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("sync pull: %s", resp.Status)
	}
	return out, json.NewDecoder(resp.Body).Decode(&out)
}
