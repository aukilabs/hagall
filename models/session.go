package models

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/aukilabs/hagall-common/logs"
	hwebsocket "github.com/aukilabs/hagall-common/websocket"
	"github.com/google/uuid"
)

// Session represents a session that contains entities and participants who can
// communicate between each other.
type Session struct {
	ID          uint32
	SessionUUID string

	AppKey string

	participantIDs   SequentialIDGenerator
	participantMutex sync.RWMutex
	participants     map[uint32]*Participant

	entityIDs   SequentialIDGenerator
	entityMutex sync.RWMutex
	entities    map[uint32]*Entity

	moduleStates map[string]any
	moduleMutex  sync.RWMutex

	startFrameOnce  sync.Once
	closeFrameChan  chan struct{}
	frameTicker     *time.Ticker
	frameHandlerIDs SequentialIDGenerator
	frameHandlers   map[uint32]func()
	frameMutex      sync.RWMutex

	entityComponents *EntityComponentStore

	closeOnce sync.Once
}

func NewSession(id uint32, frameDuration time.Duration) *Session {
	return &Session{
		ID:               id,
		SessionUUID:      uuid.New().String(),
		closeFrameChan:   make(chan struct{}, 1),
		frameTicker:      time.NewTicker(frameDuration),
		participants:     make(map[uint32]*Participant),
		entities:         make(map[uint32]*Entity),
		moduleStates:     make(map[string]any),
		frameHandlers:    make(map[uint32]func()),
		entityComponents: newEntityComponentStore(),
	}
}

func (s *Session) Close() {
	s.closeOnce.Do(func() {
		s.frameTicker.Stop()
		s.closeFrameChan <- struct{}{}
	})
}

func (s *Session) NewParticipantID() uint32 {
	return s.participantIDs.New()
}

func (s *Session) AddParticipant(p *Participant) {
	s.participantMutex.Lock()
	defer s.participantMutex.Unlock()

	s.participants[p.ID] = p
}

func (s *Session) RemoveParticipant(p *Participant) {
	s.participantMutex.Lock()
	defer s.participantMutex.Unlock()

	delete(s.participants, p.ID)
}

func (s *Session) GetParticipants() []*Participant {
	s.participantMutex.RLock()
	defer s.participantMutex.RUnlock()

	participants := make([]*Participant, 0, len(s.participants))
	for _, p := range s.participants {
		participants = append(participants, p)
	}
	return participants
}

func (s *Session) GetParticipantsByIDs(ids ...uint32) []*Participant {
	s.participantMutex.RLock()
	defer s.participantMutex.RUnlock()

	participants := make([]*Participant, 0, len(ids))
	for _, id := range ids {
		p, ok := s.participants[id]
		if ok {
			participants = append(participants, p)
		}
	}
	return participants
}

func (s *Session) ParticipantCount() int {
	s.participantMutex.RLock()
	defer s.participantMutex.RUnlock()

	return len(s.participants)
}

func (s *Session) NewEntityID() uint32 {
	return s.entityIDs.New()
}

func (s *Session) AddEntity(e *Entity) {
	s.entityMutex.Lock()
	defer s.entityMutex.Unlock()

	s.entities[e.ID] = e
}

func (s *Session) RemoveEntity(e *Entity) {
	s.entityMutex.Lock()
	defer s.entityMutex.Unlock()

	delete(s.entities, e.ID)
}

func (s *Session) EntityByID(id uint32) (*Entity, bool) {
	s.entityMutex.RLock()
	defer s.entityMutex.RUnlock()

	e, ok := s.entities[id]
	return e, ok
}

func (s *Session) Entities() []*Entity {
	s.entityMutex.RLock()
	defer s.entityMutex.RUnlock()

	entities := make([]*Entity, 0, len(s.entities))
	for _, e := range s.entities {
		entities = append(entities, e)
	}
	return entities
}

func (s *Session) Broadcast(sender *Participant, protoMsg hwebsocket.ProtoMsg) {
	s.participantMutex.RLock()
	defer s.participantMutex.RUnlock()

	msg, err := hwebsocket.MsgFromProto(protoMsg)
	if err != nil {
		logs.WithTag("message", protoMsg).Debug(err)
		return
	}

	for _, p := range s.participants {
		if p == sender {
			continue
		}
		p.Responder.SendMsg(msg)
	}
}

func (s *Session) BroadcastTo(sender *Participant, protoMsg hwebsocket.ProtoMsg, participantIds ...uint32) {
	participants := s.GetParticipantsByIDs(participantIds...)
	isParticipantHandled := make(map[uint32]struct{}, len(participantIds))

	msg, err := hwebsocket.MsgFromProto(protoMsg)
	if err != nil {
		logs.WithTag("message", protoMsg).Debug(err)
		return
	}

	for _, p := range participants {
		if p == sender {
			continue
		}

		if _, ok := isParticipantHandled[p.ID]; ok {
			continue
		}
		isParticipantHandled[p.ID] = struct{}{}

		p.Responder.SendMsg(msg)
	}
}

func (s *Session) SetModuleState(moduleName string, state any) {
	s.moduleMutex.Lock()
	defer s.moduleMutex.Unlock()

	s.moduleStates[moduleName] = state
}

func (s *Session) ModuleState(moduleName string) (any, bool) {
	s.moduleMutex.RLock()
	defer s.moduleMutex.RUnlock()

	state, ok := s.moduleStates[moduleName]
	return state, ok
}

func (s *Session) HandleFrame(h func()) (cancel func()) {
	s.frameMutex.Lock()
	defer s.frameMutex.Unlock()

	id := s.frameHandlerIDs.New()
	s.frameHandlers[id] = h

	return func() {
		s.frameMutex.Lock()
		defer s.frameMutex.Unlock()

		delete(s.frameHandlers, id)
		s.frameHandlerIDs.Reuse(id)
	}
}

func (s *Session) StartDispatchFrames() {
	s.startFrameOnce.Do(func() {
		for {
			select {
			case <-s.closeFrameChan:
				return

			case <-s.frameTicker.C:
				s.frameMutex.RLock()
				for _, h := range s.frameHandlers {
					h()
				}
				s.frameMutex.RUnlock()
			}
		}
	})
}

func (s *Session) GetEntityComponents() *EntityComponentStore {
	return s.entityComponents
}

type SessionStore struct {
	// The session discovery service where sessions are registered.
	DiscoveryService SessionDiscoveryService

	initOnce sync.Once
	mutex    sync.RWMutex
	sessions map[string]*Session
	ids      SequentialIDGenerator
}

func (s *SessionStore) init() {
	s.sessions = map[string]*Session{}

	if s.DiscoveryService == nil {
		s.DiscoveryService = defaultSessionDiscoveryService{}
	}
}

func (s *SessionStore) NewID() uint32 {
	return s.ids.New()
}

func (s *SessionStore) Add(ctx context.Context, session *Session) error {
	s.initOnce.Do(s.init)
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.sessions[s.GlobalSessionID(session.ID)] = session

	instrumentIncreaseSessionGauge(session.AppKey)
	instrumentCountSession(session.AppKey)
	return nil
}

func (s *SessionStore) Remove(ctx context.Context, session *Session) {
	s.initOnce.Do(s.init)
	s.mutex.Lock()
	defer s.mutex.Unlock()

	delete(s.sessions, s.GlobalSessionID(session.ID))
	session.Close()

	s.ids.Reuse(session.ID)

	instrumentDecreaseSessionGauge(session.AppKey)
}

func (s *SessionStore) GetByGlobalID(v string) (*Session, bool) {
	s.initOnce.Do(s.init)

	s.mutex.RLock()
	defer s.mutex.RUnlock()

	session, ok := s.sessions[v]
	return session, ok
}

func (s *SessionStore) GlobalSessionID(sessionID uint32) string {
	return fmt.Sprintf("%sx%x", s.DiscoveryService.ServerID(), sessionID)
}

// SessionDiscoveryService is the interface to communicate with a session discovery
// service such as HDS.
type SessionDiscoveryService interface {
	// Returns the id attributed to the current Hagall server.
	ServerID() string
}

type defaultSessionDiscoveryService struct{}

func (s defaultSessionDiscoveryService) ServerID() string {
	return "ted"
}
