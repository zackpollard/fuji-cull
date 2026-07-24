package cull

import "github.com/zack/fuji-tools/internal/photo"

// Catalog is the ordered list of shots found on the camera.
type Catalog struct {
	Shots []*photo.Shot
	Index map[string]int // shot ID -> position in Shots

	// Canonical/Legacy bridge the backend-local Shot.ID and the device-independent
	// canonical sync key. Canonical is the OUTBOUND resolver (a local decision on
	// shot ID -> the canonical key to push); Legacy is the INBOUND resolver (a
	// pulled canonical key -> every local ID to project the decision onto — 1:N
	// because dual-slot backup twins share one canonical key).
	Canonical map[string]string   // legacyID -> canonicalKey
	Legacy    map[string][]string // canonicalKey -> []legacyID
}

// CanonicalOf returns the canonical sync key for a backend-local shot ID.
func (c *Catalog) CanonicalOf(id string) string { return c.Canonical[id] }

// LegacyOf returns every backend-local shot ID sharing a canonical key.
func (c *Catalog) LegacyOf(ckey string) []string { return c.Legacy[ckey] }

func (c *Catalog) Get(id string) *photo.Shot {
	if i, ok := c.Index[id]; ok {
		return c.Shots[i]
	}
	return nil
}
