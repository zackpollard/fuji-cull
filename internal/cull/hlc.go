package cull

import (
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/zack/fuji-tools/internal/synccore"
)

// hlc aliases the shared clock type so the engine and the sync server order and
// merge records by the exact same rule (synccore is the single authority).
type hlc = synccore.HLC

func maxHLC(a, b hlc) hlc { return synccore.Max(a, b) }

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
