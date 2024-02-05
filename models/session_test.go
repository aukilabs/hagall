package models

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/aukilabs/hagall-common/messages/hagallpb"
	hwebsocket "github.com/aukilabs/hagall-common/websocket"
	"github.com/stretchr/testify/require"
)

func TestSessionNewParticipantID(t *testing.T) {
	session := NewSession(42, time.Second)
	require.NotZero(t, session.NewParticipantID())
}

func TestSessionAddParticipant(t *testing.T) {
	participant := &Participant{ID: 777}
	session := NewSession(42, time.Second)

	session.AddParticipant(participant)
	require.Len(t, session.participants, 1)
	require.Equal(t, participant, session.participants[777])
}

func TestSessionRemoveParticipant(t *testing.T) {
	participant := &Participant{ID: 777}
	session := NewSession(42, time.Second)

	session.AddParticipant(participant)
	require.Len(t, session.participants, 1)

	session.RemoveParticipant(participant)
	require.Empty(t, session.participants)
}

func TestSessionGetParticipants(t *testing.T) {
	participant := &Participant{ID: 777}
	session := NewSession(42, time.Second)

	session.AddParticipant(participant)

	participants := session.GetParticipants()
	require.Len(t, participants, 1)
	require.Equal(t, participant, participants[0])
}

func TestSessionGetParticipantsByIDs(t *testing.T) {
	session := NewSession(42, time.Second)

	for i := 1; i <= 10; i++ {
		session.AddParticipant(&Participant{ID: uint32(i)})
	}

	participants := session.GetParticipantsByIDs(3, 7)
	require.Len(t, participants, 2)

	sort.Slice(participants, func(i, j int) bool {
		return participants[i].ID < participants[j].ID
	})

	require.Equal(t, uint32(3), participants[0].ID)
	require.Equal(t, uint32(7), participants[1].ID)
}

func TestSessionNewEntityID(t *testing.T) {
	session := Session{}
	require.NotZero(t, session.NewEntityID())
}

func TestSessionAddEntity(t *testing.T) {
	entity := &Entity{ID: 11}
	session := NewSession(42, time.Second)

	session.AddEntity(entity)
	require.Len(t, session.entities, 1)
	require.Equal(t, entity, session.entities[11])
}

func TestSessionRemoveEntity(t *testing.T) {
	t.Run("remove entity", func(t *testing.T) {
		entity := &Entity{ID: 11}
		session := NewSession(42, time.Second)

		session.AddEntity(entity)
		require.Len(t, session.entities, 1)

		session.RemoveEntity(entity)
		require.Empty(t, session.entities)
	})
}

func TestSessionEntityByID(t *testing.T) {
	session := NewSession(42, time.Second)

	t.Run("entity is returned", func(t *testing.T) {
		entity := &Entity{ID: 1}
		session.AddEntity(entity)

		rEntity, ok := session.EntityByID(entity.ID)
		require.True(t, ok)
		require.Equal(t, entity, rEntity)
	})

	t.Run("entity is not returned", func(t *testing.T) {
		rEntity, ok := session.EntityByID(2)
		require.False(t, ok)
		require.Nil(t, rEntity)
	})
}

func TestSessionEntities(t *testing.T) {
	entity := &Entity{ID: 1}
	session := NewSession(42, time.Second)

	session.AddEntity(entity)

	entities := session.Entities()
	require.Len(t, entities, 1)
	require.Equal(t, entity, entities[0])
}

func TestSessionModuleState(t *testing.T) {
	t.Run("module state is found", func(t *testing.T) {
		s := NewSession(42, time.Second)

		stateA := 42
		s.SetModuleState("testModule", stateA)

		stateB, ok := s.ModuleState("testModule")
		require.True(t, ok)
		require.Equal(t, stateA, stateB)
	})

	t.Run("module state is not found", func(t *testing.T) {
		s := NewSession(42, time.Second)

		state, ok := s.ModuleState("testModule")
		require.False(t, ok)
		require.Nil(t, state)
	})
}

func TestSessionBroadcast(t *testing.T) {
	t.Run("msg from participant A is broadcasted to participant B", func(t *testing.T) {
		var sendACalled bool
		participantA := &Participant{
			ID: 1,
			Responder: testResponseSender{
				sendMsg: func(_ hwebsocket.Msg) {
					sendACalled = true
				},
				send: func(_ hwebsocket.ProtoMsg) {},
			},
		}

		var sendBCalled bool
		participantB := &Participant{
			ID: 2,
			Responder: testResponseSender{
				sendMsg: func(_ hwebsocket.Msg) {
					sendBCalled = true
				},
				send: func(_ hwebsocket.ProtoMsg) {},
			},
		}

		session := NewSession(42, time.Second)
		session.AddParticipant(participantA)
		session.AddParticipant(participantB)

		session.Broadcast(participantA, &hagallpb.Msg{})
		require.False(t, sendACalled)
		require.True(t, sendBCalled)
	})
}

func TestBroadcastTo(t *testing.T) {
	t.Run("message is not broadcasted to sender", func(t *testing.T) {
		var sendACalled bool
		participantA := &Participant{
			ID: 1,
			Responder: testResponseSender{
				sendMsg: func(_ hwebsocket.Msg) {
					sendACalled = true
				},
				send: func(_ hwebsocket.ProtoMsg) {},
			},
		}

		session := NewSession(42, time.Second)
		session.AddParticipant(participantA)

		session.BroadcastTo(participantA, &hagallpb.Msg{}, participantA.ID)
		require.False(t, sendACalled)
	})

	t.Run("message is broadcasted to participant B", func(t *testing.T) {
		var sendACalled bool
		participantA := &Participant{
			ID: 1,
			Responder: testResponseSender{
				sendMsg: func(_ hwebsocket.Msg) {
					sendACalled = true
				},
				send: func(_ hwebsocket.ProtoMsg) {},
			},
		}

		var sendBCalled bool
		participantB := &Participant{
			ID: 2,
			Responder: testResponseSender{
				sendMsg: func(_ hwebsocket.Msg) {
					sendBCalled = true
				},
				send: func(_ hwebsocket.ProtoMsg) {},
			},
		}

		session := NewSession(42, time.Second)
		session.AddParticipant(participantA)
		session.AddParticipant(participantB)

		session.BroadcastTo(participantA, &hagallpb.Msg{}, participantB.ID)
		require.False(t, sendACalled)
		require.True(t, sendBCalled)
	})

	t.Run("message is broadcasted to participant B once", func(t *testing.T) {
		var sendACalled bool
		participantA := &Participant{
			ID: 1,
			Responder: testResponseSender{
				sendMsg: func(_ hwebsocket.Msg) {
					sendACalled = true
				},
				send: func(_ hwebsocket.ProtoMsg) {},
			},
		}

		var sendBCalls int
		participantB := &Participant{
			ID: 2,
			Responder: testResponseSender{
				sendMsg: func(_ hwebsocket.Msg) {
					sendBCalls++
				},
				send: func(_ hwebsocket.ProtoMsg) {},
			},
		}

		session := NewSession(42, time.Second)
		session.AddParticipant(participantA)
		session.AddParticipant(participantB)

		session.BroadcastTo(participantA, &hagallpb.Msg{},
			participantB.ID,
			participantB.ID,
			participantB.ID,
			participantB.ID,
		)
		require.False(t, sendACalled)
		require.Equal(t, 1, sendBCalls)
	})

	t.Run("message to unknown participant is skipped", func(t *testing.T) {
		var sendACalled bool
		participantA := &Participant{
			ID: 1,
			Responder: testResponseSender{
				sendMsg: func(_ hwebsocket.Msg) {
					sendACalled = true
				},
				send: func(_ hwebsocket.ProtoMsg) {},
			},
		}

		session := NewSession(42, time.Second)
		session.AddParticipant(participantA)

		session.BroadcastTo(participantA, &hagallpb.Msg{}, 42)
		require.False(t, sendACalled)
	})
}

func TestSessionStoreNewID(t *testing.T) {
	sessions := SessionStore{}
	require.NotZero(t, sessions.NewID())
}

func TestSessionStoreAdd(t *testing.T) {
	t.Run("session is successfully added", func(t *testing.T) {
		var sessions SessionStore

		session := NewSession(42, time.Second)

		err := sessions.Add(context.Background(), session)
		require.NoError(t, err)
		require.Equal(t, session, sessions.sessions[sessions.GlobalSessionID(session.ID)])
	})
}

func TestSessionStoreRemove(t *testing.T) {

	t.Run("session is successfully removed", func(t *testing.T) {
		var sessions SessionStore

		ctx := context.Background()

		session := NewSession(42, time.Second)
		err := sessions.Add(ctx, session)
		require.NoError(t, err)
		require.Len(t, sessions.sessions, 1)

		sessions.Remove(ctx, session)
		require.Empty(t, sessions.sessions)
	})

	t.Run("session id is reused", func(t *testing.T) {
		var sessions SessionStore

		ctx := context.Background()

		sessionID := sessions.NewID()
		session := NewSession(sessionID, time.Second)
		err := sessions.Add(ctx, session)
		require.NoError(t, err)
		require.Len(t, sessions.sessions, 1)

		sessions.Remove(ctx, session)
		require.Empty(t, sessions.sessions)

		nextSessionID := sessions.NewID()
		require.Equal(t, sessionID, nextSessionID)
	})
}

func TestSessionStoreGetByGlobalID(t *testing.T) {
	var sessions SessionStore
	ctx := context.Background()

	t.Run("session is retrieved", func(t *testing.T) {
		session := NewSession(42, time.Second)
		err := sessions.Add(ctx, session)
		require.NoError(t, err)

		res, ok := sessions.GetByGlobalID(sessions.GlobalSessionID(session.ID))
		require.True(t, ok)
		require.Equal(t, session, res)
	})

	t.Run("session is not retrieved", func(t *testing.T) {
		session := &Session{ID: 84}
		res, ok := sessions.GetByGlobalID(sessions.GlobalSessionID(session.ID))
		require.False(t, ok)
		require.Nil(t, res)
	})
}

func TestSessionHandleFrame(t *testing.T) {
	session := NewSession(42, time.Millisecond*5)

	cancel := session.HandleFrame(func() {})
	require.Len(t, session.frameHandlers, 1)
	defer cancel()

	cancel()
	require.Empty(t, session.frameHandlers)

}

func TestSessionStartDispatchFrame(t *testing.T) {
	session := NewSession(42, time.Millisecond*5)

	var wg sync.WaitGroup
	wg.Add(1)

	go session.StartDispatchFrames()

	session.HandleFrame(func() {
		wg.Done()
	})

	wg.Wait()
	session.Close()
}

type testResponseSender struct {
	send    func(hwebsocket.ProtoMsg)
	sendMsg func(hwebsocket.Msg)
}

func (r testResponseSender) Send(protoMsg hwebsocket.ProtoMsg) {
	r.send(protoMsg)
}

func (r testResponseSender) SendMsg(msg hwebsocket.Msg) {
	r.sendMsg(msg)
}
