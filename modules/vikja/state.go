package vikja

import (
	"sync"

	"github.com/aukilabs/hagall-common/messages/vikjapb"
)

type State struct {
	entityActionMutex sync.RWMutex
	entityActions     map[uint32]map[string]*vikjapb.EntityAction
}

func (s *State) SetEntityAction(ea *vikjapb.EntityAction) {
	s.entityActionMutex.Lock()
	defer s.entityActionMutex.Unlock()

	if s.entityActions == nil {
		s.entityActions = make(map[uint32]map[string]*vikjapb.EntityAction)
	}

	entityActions, ok := s.entityActions[ea.EntityId]
	if !ok {
		entityActions = make(map[string]*vikjapb.EntityAction)
		s.entityActions[ea.EntityId] = entityActions
	}

	entityActions[ea.Name] = ea
}

func (s *State) EntityAction(entityID uint32, actionName string) (*vikjapb.EntityAction, bool) {
	s.entityActionMutex.RLock()
	defer s.entityActionMutex.RUnlock()

	entityActions, ok := s.entityActions[entityID]
	if !ok {
		return nil, false
	}

	entityAction, ok := entityActions[actionName]
	return entityAction, ok
}

func (s *State) RemoveEntityActions(entityID uint32) {
	s.entityActionMutex.Lock()
	defer s.entityActionMutex.Unlock()

	delete(s.entityActions, entityID)
}

func (s *State) EntityActions() []*vikjapb.EntityAction {
	s.entityActionMutex.RLock()
	defer s.entityActionMutex.RUnlock()

	entityActions := make([]*vikjapb.EntityAction, 0, len(s.entityActions))
	for _, eas := range s.entityActions {
		for _, ea := range eas {
			entityActions = append(entityActions, ea)
		}
	}
	return entityActions
}
