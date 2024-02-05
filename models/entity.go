package models

import (
	"sync"

	"github.com/aukilabs/hagall-common/errors"
	"github.com/aukilabs/hagall-common/messages/hagallpb"
	hwebsocket "github.com/aukilabs/hagall-common/websocket"
)

type Entity struct {
	ID            uint32
	ParticipantID uint32
	Persist       bool
	Flag          hagallpb.EntityFlag

	mutex sync.RWMutex
	pose  Pose
}

func (e *Entity) SetPose(v Pose) {
	e.mutex.Lock()
	defer e.mutex.Unlock()

	e.pose = v
}

func (e *Entity) Pose() Pose {
	e.mutex.RLock()
	defer e.mutex.RUnlock()

	return e.pose
}

func (e *Entity) ToProtobuf() *hagallpb.Entity {
	e.mutex.RLock()
	defer e.mutex.RUnlock()

	return &hagallpb.Entity{
		Id:            e.ID,
		ParticipantId: e.ParticipantID,
		Pose:          e.pose.ToProtobuf(),
		Flag:          e.Flag,
	}
}

func EntitiesToProtobuf(entities []*Entity) []*hagallpb.Entity {
	pEntitites := make([]*hagallpb.Entity, len(entities))
	for i, e := range entities {
		pEntitites[i] = e.ToProtobuf()
	}
	return pEntitites
}

type Pose struct {
	PX float32
	PY float32
	PZ float32
	RX float32
	RY float32
	RZ float32
	RW float32
}

func (p Pose) ToProtobuf() *hagallpb.Pose {
	return &hagallpb.Pose{
		Px: p.PX,
		Py: p.PY,
		Pz: p.PZ,
		Rx: p.RX,
		Ry: p.RY,
		Rz: p.RZ,
		Rw: p.RW,
	}
}

type EntityComponentHandler func([]uint32)

type EntityComponentStore struct {
	mutex            sync.RWMutex
	ids              SequentialIDGenerator
	nameIndex        map[uint32]string
	idIndex          map[string]uint32
	entityComponents map[uint32]map[uint32]*hagallpb.EntityComponent

	subscriptionMutex sync.RWMutex
	subscriptions     map[uint32]map[uint32]struct{}
}

func newEntityComponentStore() *EntityComponentStore {
	return &EntityComponentStore{
		nameIndex:        make(map[uint32]string),
		idIndex:          make(map[string]uint32),
		entityComponents: make(map[uint32]map[uint32]*hagallpb.EntityComponent),
		subscriptions:    make(map[uint32]map[uint32]struct{}),
	}
}

func (s *EntityComponentStore) AddType(name string) uint32 {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if eaID, ok := s.idIndex[name]; ok {
		return eaID
	}

	id := s.ids.New()
	s.nameIndex[id] = name
	s.idIndex[name] = id
	return id
}

func (s *EntityComponentStore) GetTypeName(entityComponentTypeID uint32) (string, error) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	name, ok := s.nameIndex[entityComponentTypeID]
	if !ok {
		return "", errors.New("entity component type is not added").WithTag("id", entityComponentTypeID)
	}
	return name, nil
}

func (s *EntityComponentStore) GetTypeID(entityComponentTypeName string) (uint32, error) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	id, ok := s.idIndex[entityComponentTypeName]
	if !ok {
		return 0, errors.New("entity component type is not added").WithTag("name", entityComponentTypeName)
	}
	return id, nil
}

func (s *EntityComponentStore) Add(ec *hagallpb.EntityComponent) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if _, ok := s.nameIndex[ec.EntityComponentTypeId]; !ok {
		return errors.New("entity component type is not added").
			WithType(hwebsocket.ErrEntityComponentTypeNotAdded).
			WithTag("id", ec.EntityComponentTypeId)
	}

	if _, ok := s.entityComponents[ec.EntityComponentTypeId]; !ok {
		s.entityComponents[ec.EntityComponentTypeId] = make(map[uint32]*hagallpb.EntityComponent)
	}

	if _, ok := s.entityComponents[ec.EntityComponentTypeId][ec.EntityId]; ok {
		return errors.New("entity component is already added").
			WithType(hwebsocket.ErrEntityComponentTypeAlreadyAdded).
			WithTag("id", ec.EntityComponentTypeId).
			WithTag("entity_id", ec.EntityId)
	}
	s.entityComponents[ec.EntityComponentTypeId][ec.EntityId] = ec

	return nil
}

func (s *EntityComponentStore) Delete(entityComponentTypeID uint32, entityID uint32) bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	entityComponents, ok := s.entityComponents[entityComponentTypeID]
	if !ok {
		return false
	}

	_, ok = entityComponents[entityID]
	delete(entityComponents, entityID)
	return ok
}

func (s *EntityComponentStore) DeleteByEntityID(entityID uint32) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	for _, ecs := range s.entityComponents {
		delete(ecs, entityID)
	}
}

func (s *EntityComponentStore) Update(ec *hagallpb.EntityComponent) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	entityComponents, ok := s.entityComponents[ec.EntityComponentTypeId]
	if !ok {
		return errors.New("entity component has not been added").
			WithTag("id", ec.EntityComponentTypeId).
			WithTag("entity_id", ec.EntityId)
	}

	_, ok = entityComponents[ec.EntityId]
	if !ok {
		return errors.New("entity component has not been added").
			WithTag("id", ec.EntityComponentTypeId).
			WithTag("entity_id", ec.EntityId)
	}

	s.entityComponents[ec.EntityComponentTypeId][ec.EntityId] = ec
	return nil
}

func (s *EntityComponentStore) List(entityComponentTypeID uint32) []*hagallpb.EntityComponent {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	if len(s.entityComponents[entityComponentTypeID]) == 0 {
		return nil
	}

	list := make([]*hagallpb.EntityComponent, 0, len(s.entityComponents[entityComponentTypeID]))
	for _, ec := range s.entityComponents[entityComponentTypeID] {
		list = append(list, ec)
	}
	return list
}

func (s *EntityComponentStore) ListByEntityID(entityID uint32) []*hagallpb.EntityComponent {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	var list []*hagallpb.EntityComponent
	for _, ecs := range s.entityComponents {
		if ec, ok := ecs[entityID]; ok {
			list = append(list, ec)
		}
	}
	return list
}

func (s *EntityComponentStore) ListAll() []*hagallpb.EntityComponent {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	var list []*hagallpb.EntityComponent
	for _, ecs := range s.entityComponents {
		for _, ec := range ecs {
			list = append(list, ec)
		}
	}
	return list
}

func (s *EntityComponentStore) Subscribe(entityComponentTypeID, participantID uint32) error {
	s.subscriptionMutex.Lock()
	defer s.subscriptionMutex.Unlock()

	s.mutex.RLock()
	defer s.mutex.RUnlock()

	if _, ok := s.nameIndex[entityComponentTypeID]; !ok {
		return errors.New("entity component type is not added").
			WithType(hwebsocket.ErrEntityComponentTypeNotAdded).
			WithTag("id", entityComponentTypeID)
	}

	if _, ok := s.subscriptions[entityComponentTypeID]; !ok {
		s.subscriptions[entityComponentTypeID] = make(map[uint32]struct{})
	}
	s.subscriptions[entityComponentTypeID][participantID] = struct{}{}
	return nil
}

func (s *EntityComponentStore) Unsubscribe(entityComponentTypeID, participantID uint32) {
	s.subscriptionMutex.Lock()
	defer s.subscriptionMutex.Unlock()

	if _, ok := s.subscriptions[entityComponentTypeID]; !ok {
		return
	}

	delete(s.subscriptions[entityComponentTypeID], participantID)
}

func (s *EntityComponentStore) UnsubscribeByParticipant(participantID uint32) {
	s.subscriptionMutex.Lock()
	defer s.subscriptionMutex.Unlock()

	for _, subscriptions := range s.subscriptions {
		delete(subscriptions, participantID)
	}
}

func (s *EntityComponentStore) Notify(entityComponentTypeID uint32, h EntityComponentHandler) {
	s.subscriptionMutex.RLock()
	defer s.subscriptionMutex.RUnlock()

	subscriptions := s.subscriptions[entityComponentTypeID]
	if len(subscriptions) == 0 {
		return
	}

	participantIDs := make([]uint32, 0, len(subscriptions))
	for participantID := range s.subscriptions[entityComponentTypeID] {
		participantIDs = append(participantIDs, participantID)
	}

	h(participantIDs)
}
