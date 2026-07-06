package main

import "github.com/zack/fuji-tools/internal/photo"

// Catalog is the ordered list of shots found on the camera.
type Catalog struct {
	Shots []*photo.Shot
	Index map[string]int // shot ID -> position in Shots
}

func (c *Catalog) Get(id string) *photo.Shot {
	if i, ok := c.Index[id]; ok {
		return c.Shots[i]
	}
	return nil
}
