// Package synccore holds the wire types and the ONE merge rule shared by the
// engine (internal/cull) and the sync server (cmd/fuji-sync). Keeping the HLC
// ordering and the LWW decision in a single place is a correctness requirement:
// if the two sides resolved conflicts differently they would never converge.
package synccore

// HLC is a Hybrid Logical Clock stamp. Ordering is lexicographic
// (Wall, Ctr, Dev, Nonce); Wall is unix-ms.
type HLC struct {
	Wall  int64  `json:"w"`
	Ctr   int64  `json:"c"`
	Dev   string `json:"d"`
	Nonce string `json:"n,omitempty"`
}

// Less is a strict total order (never true for equal clocks).
func (a HLC) Less(b HLC) bool {
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

func (a HLC) IsZero() bool { return a.Wall == 0 && a.Ctr == 0 && a.Dev == "" }

// Max returns whichever clock sorts higher.
func Max(a, b HLC) HLC {
	if a.Less(b) {
		return b
	}
	return a
}

// Wins reports whether an incoming record should replace a stored one. Rule:
//   - a record with no stored counterpart always wins;
//   - a genuine (non-migrated) record always beats a migrated one, regardless of
//     clock — migrated values only ever fill gaps;
//   - otherwise the higher HLC wins.
//
// This is the single merge authority; the engine and server both call it.
func Wins(inHLC HLC, inMigrated bool, stHLC HLC, stMigrated, stExists bool) bool {
	if !stExists {
		return true
	}
	if inMigrated != stMigrated {
		return !inMigrated
	}
	return stHLC.Less(inHLC)
}

// ── wire types ────────────────────────────────────────────────────────────

// DecisionRow is one decision as it crosses the wire (push item or pull delta).
type DecisionRow struct {
	Ckey      string `json:"ckey"`
	D         string `json:"d"`
	Del       bool   `json:"del,omitempty"`
	HLC       HLC    `json:"hlc"`
	Migrated  bool   `json:"migrated,omitempty"`
	Version   int64  `json:"version,omitempty"`   // server-assigned
	Contested bool   `json:"contested,omitempty"` // set by the server on a suspected skew clash
}

// ResumeRow is one per-device resume point.
type ResumeRow struct {
	Dev     string `json:"dev"`
	Ckey    string `json:"ckey"`
	HLC     HLC    `json:"hlc"`
	Version int64  `json:"version,omitempty"`
}

// PushRequest uploads a device's dirty records + its resume point.
type PushRequest struct {
	Camera    string        `json:"camera"`
	DeviceID  string        `json:"deviceId"`
	Epoch     string        `json:"epoch"` // last epoch the client saw ("" if never synced)
	Decisions []DecisionRow `json:"decisions"`
	Resume    *ResumeRow    `json:"resume,omitempty"`
}

// AckRow is the server's authoritative winner for one pushed key.
type AckRow struct {
	Ckey      string      `json:"ckey"`
	Accepted  bool        `json:"accepted"`
	Version   int64       `json:"version"`
	Winner    DecisionRow `json:"winner"`
	Contested bool        `json:"contested,omitempty"`
}

// PushResponse returns per-item winners + the camera high-water + generation.
type PushResponse struct {
	Epoch         string   `json:"epoch"`
	CameraVersion int64    `json:"cameraVersion"`
	ServerNow     int64    `json:"serverNow"`
	Results       []AckRow `json:"results"`
}

// PullResponse returns deltas strictly above the client's `since` high-water.
type PullResponse struct {
	Epoch     string        `json:"epoch"`
	DeltaHigh int64         `json:"deltaHigh"` // max version among returned rows; the client's next `since`
	ServerNow int64         `json:"serverNow"`
	Decisions []DecisionRow `json:"decisions"`
	Resume    []ResumeRow   `json:"resume"`
}
