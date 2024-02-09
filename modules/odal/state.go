package odal

import (
	"sync"

	"github.com/aukilabs/hagall-common/messages/odalpb"
	"github.com/aukilabs/hagall/models"
)

// State represents a state that keeps track of assets instances.
type State struct {
	assetMutex       sync.RWMutex
	assetInstanceIDs models.SequentialIDGenerator
	assetInstances   map[uint32]*odalpb.AssetInstance
}

func (s *State) NewAssetInstanceID() uint32 {
	return s.assetInstanceIDs.New()
}

func (s *State) SetAssetInstance(ai *odalpb.AssetInstance) {
	s.assetMutex.Lock()
	defer s.assetMutex.Unlock()

	if s.assetInstances == nil {
		s.assetInstances = make(map[uint32]*odalpb.AssetInstance)
	}

	s.assetInstances[ai.EntityId] = ai
}

func (s *State) RemoveAssetInstance(entityID uint32) {
	s.assetMutex.Lock()
	defer s.assetMutex.Unlock()

	delete(s.assetInstances, entityID)
}

func (s *State) AssetInstance(entityID uint32) (*odalpb.AssetInstance, bool) {
	s.assetMutex.RLock()
	defer s.assetMutex.RUnlock()

	ai, ok := s.assetInstances[entityID]
	return ai, ok
}

func (s *State) AssetInstances() []*odalpb.AssetInstance {
	s.assetMutex.RLock()
	defer s.assetMutex.RUnlock()

	assetInstances := make([]*odalpb.AssetInstance, 0, len(s.assetInstances))
	for _, ai := range s.assetInstances {
		assetInstances = append(assetInstances, ai)
	}
	return assetInstances
}
