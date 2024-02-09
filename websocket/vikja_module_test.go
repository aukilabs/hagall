package websocket

import (
	"context"
	"testing"
	"time"

	"github.com/aukilabs/hagall-common/messages/hagallpb"
	"github.com/aukilabs/hagall-common/messages/vikjapb"
	"github.com/aukilabs/hagall-common/scenario"
	hwebsocket "github.com/aukilabs/hagall-common/websocket"
	"github.com/aukilabs/hagall/modules"
	"github.com/aukilabs/hagall/modules/vikja"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestVikjaHandleParticipantJoin(t *testing.T) {
	clientA, clientB, close := NewTestingEnv(t, newTestHandler(newVikjaTestModule))
	defer close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	var sessionID string
	entityAction := &vikjapb.EntityAction{
		Name:      "Ted's Tiger Claws",
		Timestamp: timestamppb.Now(),
	}

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
			func(msg hwebsocket.Msg) error {
				var res hagallpb.ParticipantJoinResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				sessionID = res.SessionId
				return err
			},
		).
		Receive(
			scenario.FilterByType(vikjapb.MsgType_MSG_TYPE_VIKJA_STATE),
			func(msg hwebsocket.Msg) error {
				var s vikjapb.State
				err := msg.DataTo(&s)
				require.NoError(t, err)

				require.Empty(t, s.EntityActions)
				return nil
			},
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
			func(msg hwebsocket.Msg) error {
				var res hagallpb.EntityAddResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				entityAction.EntityId = res.EntityId
				return err
			},
		).
		Send(func() hwebsocket.ProtoMsg {
			return &vikjapb.EntityActionRequest{
				Type:         vikjapb.MsgType_MSG_TYPE_VIKJA_ENTITY_ACTION_REQUEST,
				Timestamp:    timestamppb.Now(),
				RequestId:    3,
				EntityAction: entityAction,
			}
		}).
		Receive(
			scenario.FilterByRequestID(3),
			scenario.FilterByType(vikjapb.MsgType_MSG_TYPE_VIKJA_ENTITY_ACTION_RESPONSE),
			func(msg hwebsocket.Msg) error {
				var res vikjapb.EntityActionResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				return err
			},
		).
		Run(ctx)
	require.NoError(t, err)

	err = scenario.NewScenario(clientB).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.ParticipantJoinRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 4,
				SessionId: sessionID,
			}
		}).
		Receive(
			scenario.FilterByType(vikjapb.MsgType_MSG_TYPE_VIKJA_STATE),
			func(msg hwebsocket.Msg) error {
				var s vikjapb.State
				err := msg.DataTo(&s)
				require.NoError(t, err)

				require.Len(t, s.EntityActions, 1)
				ea := s.EntityActions[0]
				require.Equal(t, entityAction.EntityId, ea.EntityId)
				require.Equal(t, entityAction.Name, ea.Name)
				require.True(t, entityAction.Timestamp.AsTime().Equal(ea.Timestamp.AsTime()))
				require.Equal(t, entityAction.Data, ea.Data)
				return nil
			},
		).
		Run(ctx)
	require.NoError(t, err)
}

func TestHandleEntityActionNoJoinedSession(t *testing.T) {
	clientA, _, close := NewTestingEnv(t, newTestHandler(newVikjaTestModule))
	defer close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := scenario.NewScenario(clientA).
		Send(func() hwebsocket.ProtoMsg {
			return &vikjapb.EntityActionRequest{
				Type:      vikjapb.MsgType_MSG_TYPE_VIKJA_ENTITY_ACTION_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 1,
			}
		}).
		Receive(func(msg hwebsocket.Msg) error {
			return scenario.ErrScenarioMsgSkip
		}).
		Run(ctx)
	require.Error(t, err)
}

func TestHandleEntityActionNil(t *testing.T) {
	clientA, _, close := NewTestingEnv(t, newTestHandler(newVikjaTestModule))
	defer close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
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
		Send(func() hwebsocket.ProtoMsg {
			return &vikjapb.EntityActionRequest{
				Type:      vikjapb.MsgType_MSG_TYPE_VIKJA_ENTITY_ACTION_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 2,
			}
		}).
		Receive(
			scenario.FilterByRequestID(2),
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.ErrorResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				require.Equal(t, hagallpb.ErrorCode_ERROR_CODE_BAD_REQUEST, res.Code)
				return err
			}).
		Run(ctx)
	require.NoError(t, err)
}

func TestHandleEntityActionMissingName(t *testing.T) {
	clientA, _, close := NewTestingEnv(t, newTestHandler(newVikjaTestModule))
	defer close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
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
		Send(func() hwebsocket.ProtoMsg {
			return &vikjapb.EntityActionRequest{
				Type:      vikjapb.MsgType_MSG_TYPE_VIKJA_ENTITY_ACTION_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 2,
				EntityAction: &vikjapb.EntityAction{
					EntityId:  42,
					Timestamp: timestamppb.Now(),
				},
			}
		}).
		Receive(
			scenario.FilterByRequestID(2),
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.ErrorResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				require.Equal(t, hagallpb.ErrorCode_ERROR_CODE_BAD_REQUEST, res.Code)
				return err
			}).
		Run(ctx)
	require.NoError(t, err)
}

func TestHandleEntityActionMissingTimestamp(t *testing.T) {
	clientA, _, close := NewTestingEnv(t, newTestHandler(newVikjaTestModule))
	defer close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
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
		Send(func() hwebsocket.ProtoMsg {
			return &vikjapb.EntityActionRequest{
				Type:      vikjapb.MsgType_MSG_TYPE_VIKJA_ENTITY_ACTION_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 2,
				EntityAction: &vikjapb.EntityAction{
					EntityId: 42,
					Name:     "Ted's Dragon Punch",
				},
			}
		}).
		Receive(
			scenario.FilterByRequestID(2),
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.ErrorResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				require.Equal(t, hagallpb.ErrorCode_ERROR_CODE_BAD_REQUEST, res.Code)
				return err
			}).
		Run(ctx)
	require.NoError(t, err)
}

func TestHandleEntityActionNoEntity(t *testing.T) {
	clientA, _, close := NewTestingEnv(t, newTestHandler(newVikjaTestModule))
	defer close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
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
		Send(func() hwebsocket.ProtoMsg {
			return &vikjapb.EntityActionRequest{
				Type:      vikjapb.MsgType_MSG_TYPE_VIKJA_ENTITY_ACTION_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 2,
				EntityAction: &vikjapb.EntityAction{
					Name:      "Ted's Dragon Punch",
					Timestamp: timestamppb.Now(),
				},
			}
		}).
		Receive(
			scenario.FilterByRequestID(2),
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.ErrorResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				require.Equal(t, hagallpb.ErrorCode_ERROR_CODE_BAD_REQUEST, res.Code)
				return err
			}).
		Run(ctx)
	require.NoError(t, err)
}

func TestHandleEntityAction(t *testing.T) {
	t.Run("set entity action by creator", func(t *testing.T) {
		clientA, _, close := NewTestingEnv(t, newTestHandler(newVikjaTestModule))
		defer close()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		var entityID uint32

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
				func(msg hwebsocket.Msg) error {
					var res hagallpb.EntityAddResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)

					entityID = res.EntityId
					return err
				},
			).
			Send(func() hwebsocket.ProtoMsg {
				return &vikjapb.EntityActionRequest{
					Type:      vikjapb.MsgType_MSG_TYPE_VIKJA_ENTITY_ACTION_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 3,
					EntityAction: &vikjapb.EntityAction{
						Name:      "Ted's Dragon Punch",
						Timestamp: timestamppb.Now(),
						EntityId:  entityID,
					},
				}
			}).
			Receive(
				scenario.FilterByRequestID(3),
				scenario.FilterByType(vikjapb.MsgType_MSG_TYPE_VIKJA_ENTITY_ACTION_RESPONSE),
			).
			Run(ctx)
		require.NoError(t, err)
	})

	t.Run("set entity action by non creator", func(t *testing.T) {
		clientA, clientB, close := NewTestingEnv(t, newTestHandler(newVikjaTestModule))
		defer close()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		var sessionID string
		var entityID uint32

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
				func(msg hwebsocket.Msg) error {
					var res hagallpb.ParticipantJoinResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)

					sessionID = res.SessionId
					return err
				},
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
				func(msg hwebsocket.Msg) error {
					var res hagallpb.EntityAddResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)

					entityID = res.EntityId
					return err
				},
			).
			Run(ctx)
		require.NoError(t, err)

		err = scenario.NewScenario(clientB).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.ParticipantJoinRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 1,
					SessionId: sessionID,
				}
			}).
			Receive(
				scenario.FilterByRequestID(1),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
			).
			Send(func() hwebsocket.ProtoMsg {
				return &vikjapb.EntityActionRequest{
					Type:      vikjapb.MsgType_MSG_TYPE_VIKJA_ENTITY_ACTION_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 2,
					EntityAction: &vikjapb.EntityAction{
						Name:      "Ted's Dragon Punch",
						Timestamp: timestamppb.Now(),
						EntityId:  entityID,
					},
				}
			}).
			Receive(
				scenario.FilterByRequestID(2),
				scenario.FilterByType(vikjapb.MsgType_MSG_TYPE_VIKJA_ENTITY_ACTION_RESPONSE),
			).
			Run(ctx)
		require.NoError(t, err)
	})
}

func TestHandleEntityActionBadTimestamp(t *testing.T) {
	clientA, _, close := NewTestingEnv(t, newTestHandler(newVikjaTestModule))
	defer close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	var entityID uint32

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
			func(msg hwebsocket.Msg) error {
				var res hagallpb.EntityAddResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				entityID = res.EntityId
				return err
			},
		).
		Send(func() hwebsocket.ProtoMsg {
			return &vikjapb.EntityActionRequest{
				Type:      vikjapb.MsgType_MSG_TYPE_VIKJA_ENTITY_ACTION_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 3,
				EntityAction: &vikjapb.EntityAction{
					Name:      "Ted's Dragon Punch",
					Timestamp: timestamppb.Now(),
					EntityId:  entityID,
				},
			}
		}).
		Receive(
			scenario.FilterByRequestID(3),
			scenario.FilterByType(vikjapb.MsgType_MSG_TYPE_VIKJA_ENTITY_ACTION_RESPONSE),
		).
		Send(func() hwebsocket.ProtoMsg {
			return &vikjapb.EntityActionRequest{
				Type:      vikjapb.MsgType_MSG_TYPE_VIKJA_ENTITY_ACTION_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 4,
				EntityAction: &vikjapb.EntityAction{
					Name:      "Ted's Dragon Punch",
					Timestamp: timestamppb.New(time.Now().Add(-time.Hour)),
					EntityId:  entityID,
				},
			}
		}).
		Receive(
			scenario.FilterByRequestID(4),
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.ErrorResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				require.Equal(t, hagallpb.ErrorCode_ERROR_CODE_BAD_REQUEST, res.Code)
				return err
			}).
		Run(ctx)
	require.NoError(t, err)
}

func TestHandleEntityDelete(t *testing.T) {
	clientA, clientB, close := NewTestingEnv(t, newTestHandler(newVikjaTestModule))
	defer close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	var sessionID string
	var entityID uint32

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
			func(msg hwebsocket.Msg) error {
				var res hagallpb.ParticipantJoinResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				sessionID = res.SessionId
				return err
			},
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
			func(msg hwebsocket.Msg) error {
				var res hagallpb.EntityAddResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				entityID = res.EntityId
				return err
			},
		).
		Send(func() hwebsocket.ProtoMsg {
			return &vikjapb.EntityActionRequest{
				Type:      vikjapb.MsgType_MSG_TYPE_VIKJA_ENTITY_ACTION_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 3,
				EntityAction: &vikjapb.EntityAction{
					EntityId:  entityID,
					Name:      "Ted's Howl Stare",
					Timestamp: timestamppb.Now(),
				},
			}
		}).
		Receive(
			scenario.FilterByRequestID(3),
			scenario.FilterByType(vikjapb.MsgType_MSG_TYPE_VIKJA_ENTITY_ACTION_RESPONSE),
		).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.EntityDeleteRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_DELETE_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 4,
				EntityId:  entityID,
			}
		}).
		Receive(
			scenario.FilterByRequestID(4),
			scenario.FilterByType(hagallpb.MsgType(hagallpb.MsgType_MSG_TYPE_ENTITY_DELETE_RESPONSE)),
		).
		Run(ctx)
	require.NoError(t, err)

	err = scenario.NewScenario(clientB).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.ParticipantJoinRequest{
				Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 5,
				SessionId: sessionID,
			}
		}).
		Receive(
			scenario.FilterByType(vikjapb.MsgType_MSG_TYPE_VIKJA_STATE),
			func(msg hwebsocket.Msg) error {
				var s vikjapb.State
				err := msg.DataTo(&s)
				require.NoError(t, err)

				require.Empty(t, s.EntityActions)
				return nil
			},
		).
		Run(ctx)
	require.NoError(t, err)
}

func TestHandleDisconnect(t *testing.T) {
	t.Run("handle disconnect", func(t *testing.T) {
		clientA, clientB, close := NewTestingEnv(t, newTestHandler(newVikjaTestModule))
		defer close()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		var sessionID string
		var entityID uint32

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
				func(msg hwebsocket.Msg) error {
					var res hagallpb.ParticipantJoinResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)

					sessionID = res.SessionId
					return err
				},
			).
			Run(ctx)
		require.NoError(t, err)

		err = scenario.NewScenario(clientB).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.ParticipantJoinRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 2,
					SessionId: sessionID,
				}
			}).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityAddRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 3,
				}
			}).
			Receive(
				scenario.FilterByRequestID(3),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.EntityAddResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)

					entityID = res.EntityId
					return err
				},
			).
			Send(func() hwebsocket.ProtoMsg {
				return &vikjapb.EntityActionRequest{
					Type:      vikjapb.MsgType_MSG_TYPE_VIKJA_ENTITY_ACTION_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 4,
					EntityAction: &vikjapb.EntityAction{
						EntityId:  entityID,
						Name:      "Ted's Howl Stare",
						Timestamp: timestamppb.Now(),
					},
				}
			}).
			Receive(
				scenario.FilterByRequestID(4),
				scenario.FilterByType(vikjapb.MsgType_MSG_TYPE_VIKJA_ENTITY_ACTION_RESPONSE),
			).
			Run(ctx)
		require.NoError(t, err)

		clientB.Close()

		err = scenario.NewScenario(clientA).
			Receive(scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_DELETE_BROADCAST)).
			Receive(scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_LEAVE_BROADCAST)).
			Run(ctx)
		require.NoError(t, err)
	})

	t.Run("cleanup entity's actions", func(t *testing.T) {
		clientA, clientB, close := NewTestingEnv(t, newTestHandler(newVikjaTestModule))
		defer close()

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
		defer cancel()

		var sessionID string
		var entityID uint32

		// clientA join & add entityAction
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
				func(msg hwebsocket.Msg) error {
					var res hagallpb.ParticipantJoinResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)

					sessionID = res.SessionId
					return err
				},
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityAddRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 3,
				}
			}).
			Receive(
				scenario.FilterByRequestID(3),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.EntityAddResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)

					entityID = res.EntityId
					return err
				},
			).
			Send(func() hwebsocket.ProtoMsg {
				return &vikjapb.EntityActionRequest{
					Type:      vikjapb.MsgType_MSG_TYPE_VIKJA_ENTITY_ACTION_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 4,
					EntityAction: &vikjapb.EntityAction{
						EntityId:  entityID,
						Name:      "Ted's Howl Stare",
						Timestamp: timestamppb.Now(),
					},
				}
			}).
			Receive(
				scenario.FilterByRequestID(4),
				scenario.FilterByType(vikjapb.MsgType_MSG_TYPE_VIKJA_ENTITY_ACTION_RESPONSE),
			).
			Run(ctx)
		require.NoError(t, err)

		// clientB join & received entityAction
		err = scenario.NewScenario(clientB).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.ParticipantJoinRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 1,
					SessionId: sessionID,
				}
			}).
			Receive(
				scenario.FilterByRequestID(1),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.ParticipantJoinResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)

					return err
				},
			).
			Receive(scenario.FilterByType(vikjapb.MsgType_MSG_TYPE_VIKJA_STATE),
				func(msg hwebsocket.Msg) error {
					var res vikjapb.State
					err := msg.DataTo(&res)
					require.NoError(t, err)

					require.Equal(t, 1, len(res.EntityActions))
					require.Equal(t, entityID, res.EntityActions[0].EntityId)
					return nil
				},
			).
			Run(ctx)
		require.NoError(t, err)

		// clientA left
		clientA.Close()

		// clientB re-join & entity action has been cleaned up
		err = scenario.NewScenario(clientB).
			Receive(scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_LEAVE_BROADCAST)).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.ParticipantJoinRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 1,
					SessionId: sessionID,
				}
			}).
			Receive(scenario.FilterByType(vikjapb.MsgType_MSG_TYPE_VIKJA_STATE),
				func(msg hwebsocket.Msg) error {
					var res vikjapb.State
					err := msg.DataTo(&res)
					require.NoError(t, err)

					require.Equal(t, 0, len(res.EntityActions))
					return nil
				},
			).
			Run(ctx)
		require.NoError(t, err)
	})

	t.Run("keep persistent entity's actions", func(t *testing.T) {
		clientA, clientB, close := NewTestingEnv(t, newTestHandler(newVikjaTestModule))
		defer close()

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
		defer cancel()

		var sessionID string
		var entityID uint32

		// clientA join & add entityAction
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
				func(msg hwebsocket.Msg) error {
					var res hagallpb.ParticipantJoinResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)

					sessionID = res.SessionId
					return err
				},
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityAddRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 3,
					Persist:   true,
				}
			}).
			Receive(
				scenario.FilterByRequestID(3),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.EntityAddResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)

					entityID = res.EntityId
					return err
				},
			).
			Send(func() hwebsocket.ProtoMsg {
				return &vikjapb.EntityActionRequest{
					Type:      vikjapb.MsgType_MSG_TYPE_VIKJA_ENTITY_ACTION_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 4,
					EntityAction: &vikjapb.EntityAction{
						EntityId:  entityID,
						Name:      "Ted's Howl Stare",
						Timestamp: timestamppb.Now(),
					},
				}
			}).
			Receive(
				scenario.FilterByRequestID(4),
				scenario.FilterByType(vikjapb.MsgType_MSG_TYPE_VIKJA_ENTITY_ACTION_RESPONSE),
			).
			Run(ctx)
		require.NoError(t, err)

		// clientB join & received entityAction
		err = scenario.NewScenario(clientB).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.ParticipantJoinRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 1,
					SessionId: sessionID,
				}
			}).
			Receive(
				scenario.FilterByRequestID(1),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
				func(msg hwebsocket.Msg) error {
					var res hagallpb.ParticipantJoinResponse
					err := msg.DataTo(&res)
					require.NoError(t, err)

					return err
				},
			).
			Receive(scenario.FilterByType(vikjapb.MsgType_MSG_TYPE_VIKJA_STATE),
				func(msg hwebsocket.Msg) error {
					var res vikjapb.State
					err := msg.DataTo(&res)
					require.NoError(t, err)

					require.Equal(t, 1, len(res.EntityActions))
					require.Equal(t, entityID, res.EntityActions[0].EntityId)
					return nil
				},
			).
			Run(ctx)
		require.NoError(t, err)

		// clientA left
		clientA.Close()

		// clientB re-join & receive persistent entity action
		err = scenario.NewScenario(clientB).
			Receive(scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_LEAVE_BROADCAST)).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.ParticipantJoinRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 1,
					SessionId: sessionID,
				}
			}).
			Receive(scenario.FilterByType(vikjapb.MsgType_MSG_TYPE_VIKJA_STATE),
				func(msg hwebsocket.Msg) error {
					var res vikjapb.State
					err := msg.DataTo(&res)
					require.NoError(t, err)

					require.Equal(t, 1, len(res.EntityActions))
					require.Equal(t, entityID, res.EntityActions[0].EntityId)
					return nil
				},
			).
			Run(ctx)
		require.NoError(t, err)

	})
}

func newVikjaTestModule() modules.Module {
	return &vikja.Module{}
}
