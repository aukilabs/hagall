package websocket

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aukilabs/hagall-common/messages/hagallpb"
	"github.com/aukilabs/hagall-common/scenario"
	hwebsocket "github.com/aukilabs/hagall-common/websocket"
	"github.com/aukilabs/hagall/models"
	"github.com/aukilabs/hagall/modules"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type testModule struct {
	currentSession     *models.Session
	currentParticipant *models.Participant
	handledMsgs        []protoreflect.Enum
	skippedMsgs        []protoreflect.Enum
	onDisconnect       func()
}

func (m *testModule) Name() string {
	return "test-module"
}

func (m *testModule) Init(s *models.Session, p *models.Participant) {
	m.currentSession = s
	m.currentParticipant = p
}

func (m *testModule) HandleMsg(ctx context.Context, sender hwebsocket.ResponseSender, msg hwebsocket.Msg) error {
	switch msg.Type {
	case hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_REQUEST:
		m.skippedMsgs = append(m.skippedMsgs, msg.Type)
		return hwebsocket.ErrModuleMsgSkip

	default:
		m.handledMsgs = append(m.handledMsgs, msg.Type)
		return nil
	}
}

func (m *testModule) HandleDisconnect() {
	if m.onDisconnect != nil {
		m.onDisconnect()
	}
}

func TestModule(t *testing.T) {
	var wg sync.WaitGroup
	var modA *testModule

	clientA, _, close := NewTestingEnv(t, newTestHandler(func() modules.Module {
		if modA == nil {
			wg.Add(1)
			modA = &testModule{
				onDisconnect: func() {
					wg.Done()
				},
			}
		}
		return modA
	}))
	defer close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond*100)
	defer cancel()

	err := scenario.NewScenario(clientA).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.ParticipantJoinRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 1,
			}
		}).
		Receive(
			scenario.FilterByRequestID(1),
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
		).
		Receive(
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_SESSION_STATE),
		).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.EntityAddRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 2,
			}
		}).
		Receive(
			scenario.FilterByRequestID(2),
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_RESPONSE),
		).
		Run(ctx)
	require.NoError(t, err)

	clientA.Close()

	wg.Wait()
	require.NotNil(t, modA.currentSession)
	require.NotNil(t, modA.currentParticipant)
	require.Len(t, modA.handledMsgs, 1)
	require.Equal(t, hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST, modA.handledMsgs[0])
	require.Len(t, modA.skippedMsgs, 1)
	require.Equal(t, hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_REQUEST, modA.skippedMsgs[0])
}
