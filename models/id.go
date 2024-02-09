package models

import "sync"

// A sequential id generator.
type SequentialIDGenerator struct {
	mutex       sync.Mutex
	currentID   uint32
	reusableIDs map[uint32]struct{}
}

// New returns a sequental id.
func (g *SequentialIDGenerator) New() uint32 {
	g.mutex.Lock()
	defer g.mutex.Unlock()

	for id := range g.reusableIDs {
		delete(g.reusableIDs, id)
		return id
	}

	g.currentID++
	return g.currentID
}

// Reuse marks the given id as reusable. Reusable ids are returned in priority
// when using New.
func (g *SequentialIDGenerator) Reuse(id uint32) {
	g.mutex.Lock()
	defer g.mutex.Unlock()

	if g.reusableIDs == nil {
		g.reusableIDs = make(map[uint32]struct{})
	}

	g.reusableIDs[id] = struct{}{}
}
