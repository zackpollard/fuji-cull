package cull

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// hlc is a Hybrid Logical Clock timestamp: wall time in ms, a logical counter to
// break ties within the same ms, the originating device, and a random nonce so
// two installs that (through a restore/clone) share a deviceID still get a total
// order. Ordering is lexicographic (Wall, Ctr, Dev, Nonce).
type hlc struct {
	Wall  int64  `json:"w"`
	Ctr   int64  `json:"c"`
	Dev   string `json:"d"`
	Nonce string `json:"n,omitempty"`
}

// less reports a strict total order. Never returns true for equal clocks.
func (a hlc) less(b hlc) bool {
	switch {
	case a.Wall != b.Wall:
		return a.Wall < b.Wall
	case a.Ctr != b.Ctr:
		return a.Ctr < b.Ctr
	case a.Dev != b.Dev:
		return a.Dev < b.Dev
	default:
		return a.Nonce < b.Nonce
	}
}

func (a hlc) isZero() bool { return a.Wall == 0 && a.Ctr == 0 && a.Dev == "" }

// maxHLC returns whichever clock sorts higher.
func maxHLC(a, b hlc) hlc {
	if a.less(b) {
		return b
	}
	return a
}

func nowMs() int64 { return time.Now().UnixMilli() }

func randNonce() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// newUUID returns a random RFC-4122 v4 UUID string, avoiding a module dependency
// (this package compiles into the gomobile iOS/Android binary).
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return hex.EncodeToString(b[0:4]) + "-" + hex.EncodeToString(b[4:6]) + "-" +
		hex.EncodeToString(b[6:8]) + "-" + hex.EncodeToString(b[8:10]) + "-" +
		hex.EncodeToString(b[10:16])
}

// tickHLC advances a node clock past `now` and emits the next event stamp for
// `dev`. The returned node clock (sans nonce) must be persisted so a clock
// step-back across a restart cannot rewind causality. `ceil` (0 = none) clamps
// the wall forward-bound to a trusted server reference, so a device with a dead
// RTC stuck in the future cannot pin the node clock and out-rank everyone.
func tickHLC(node hlc, dev string, now, ceil int64) (stamp, next hlc) {
	wall := now
	if node.Wall > wall {
		wall = node.Wall
	}
	if ceil > 0 && wall > ceil {
		wall = ceil
	}
	var ctr int64
	if wall == node.Wall {
		ctr = node.Ctr + 1
	}
	stamp = hlc{Wall: wall, Ctr: ctr, Dev: dev, Nonce: randNonce()}
	next = hlc{Wall: wall, Ctr: ctr, Dev: dev}
	return stamp, next
}
