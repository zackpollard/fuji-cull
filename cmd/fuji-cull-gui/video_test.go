package main

import (
	"net/url"
	"strings"
	"testing"
)

// --engine-key gates every /api/ path, and mpv cannot send a request header, so
// the key must ride in the query string. Without it the engine 401s its own
// player and video dies in the desktop app the moment LAN access is enabled.
func TestVideoURLCarriesTheEngineKey(t *testing.T) {
	const id = "SLOT 1/DCIM/156_FUJI/DSCF5754"

	got := videoURL("http://127.0.0.1:8788", "", id)
	if strings.Contains(got, "key=") {
		t.Errorf("no engine key configured, but URL carries one: %s", got)
	}

	got = videoURL("http://127.0.0.1:8788", "password", id)
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("unparseable URL %q: %v", got, err)
	}
	q := u.Query()
	if q.Get("key") != "password" {
		t.Errorf("key = %q, want %q (the engine will answer 401)", q.Get("key"), "password")
	}
	// the id must survive intact: it carries a space and slashes
	if q.Get("id") != id {
		t.Errorf("id = %q, want %q", q.Get("id"), id)
	}
}

// A key with URL-significant characters must not corrupt the query.
func TestVideoURLEscapesAwkwardKeys(t *testing.T) {
	const id = "SLOT 1/DCIM/156_FUJI/DSCF0001"
	for _, key := range []string{"a&b=c", "p ss/word", "k#1", "+plus"} {
		u, err := url.Parse(videoURL("http://127.0.0.1:8788", key, id))
		if err != nil {
			t.Fatalf("key %q gave an unparseable URL: %v", key, err)
		}
		if got := u.Query().Get("key"); got != key {
			t.Errorf("key %q round-tripped as %q", key, got)
		}
		if got := u.Query().Get("id"); got != id {
			t.Errorf("key %q corrupted the id: %q", key, got)
		}
	}
}
