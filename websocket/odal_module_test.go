package websocket

import (
	"context"
	"testing"
	"time"

	"github.com/aukilabs/hagall-common/messages/hagallpb"
	"github.com/aukilabs/hagall-common/messages/odalpb"
	"github.com/aukilabs/hagall-common/scenario"
	hwebsocket "github.com/aukilabs/hagall-common/websocket"
	"github.com/aukilabs/hagall/modules"
	"github.com/aukilabs/hagall/modules/odal"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestHandleParticipantJoin(t *testing.T) {
	clientA, clientB, close := NewTestingEnv(t, newTestHandler(newOdalTestModule))
	defer close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	var sessionID string
	assetInstance := &odalpb.AssetInstance{
		AssetId: "Ted's piano",
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
				assetInstance.ParticipantId = res.ParticipantId
				return err
			},
		).
		Receive(
			scenario.FilterByType(odalpb.MsgType_MSG_TYPE_ODAL_STATE),
			func(msg hwebsocket.Msg) error {
				var s odalpb.State
				err := msg.DataTo(&s)
				require.NoError(t, err)

				require.Empty(t, s.AssetInstances)
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

				assetInstance.EntityId = res.EntityId
				return err
			},
		).
		Send(func() hwebsocket.ProtoMsg {
			return &odalpb.AssetInstanceAddRequest{
				Type:      odalpb.MsgType_MSG_TYPE_ODAL_ASSET_INSTANCE_ADD_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 3,
				EntityId:  assetInstance.EntityId,
				AssetId:   assetInstance.AssetId,
			}
		}).
		Receive(
			scenario.FilterByRequestID(3),
			scenario.FilterByType(odalpb.MsgType_MSG_TYPE_ODAL_ASSET_INSTANCE_ADD_RESPONSE),
			func(msg hwebsocket.Msg) error {
				var res odalpb.AssetInstanceAddResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				assetInstance.Id = res.AssetInstanceId
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
			scenario.FilterByType(odalpb.MsgType_MSG_TYPE_ODAL_STATE),
			func(msg hwebsocket.Msg) error {
				var s odalpb.State
				err := msg.DataTo(&s)
				require.NoError(t, err)

				require.Len(t, s.AssetInstances, 1)
				require.Equal(t, assetInstance, s.AssetInstances[0])
				return nil
			},
		).
		Run(ctx)
	require.NoError(t, err)
}

func TestHandleAssetInstanceAddNoJoinedSession(t *testing.T) {
	clientA, _, close := NewTestingEnv(t, newTestHandler(newOdalTestModule))
	defer close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := scenario.NewScenario(clientA).
		Send(func() hwebsocket.ProtoMsg {
			return &hagallpb.ParticipantJoinRequest{
				Type:      hagallpb.MsgType(odalpb.MsgType_MSG_TYPE_ODAL_ASSET_INSTANCE_ADD_REQUEST),
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

func TestHandleAssetInstanceAddNoAssetID(t *testing.T) {
	clientA, _, close := NewTestingEnv(t, newTestHandler(newOdalTestModule))
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
			return &odalpb.AssetInstanceAddRequest{
				Type:      odalpb.MsgType_MSG_TYPE_ODAL_ASSET_INSTANCE_ADD_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 3,
				EntityId:  entityID,
			}
		}).
		Receive(
			scenario.FilterByRequestID(3),
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

func TestHandleAssetInstanceAddNoEntity(t *testing.T) {
	clientA, _, close := NewTestingEnv(t, newTestHandler(newOdalTestModule))
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
			return &odalpb.AssetInstanceAddRequest{
				Type:      odalpb.MsgType_MSG_TYPE_ODAL_ASSET_INSTANCE_ADD_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 2,
				AssetId:   "Ted's tent",
			}
		}).
		Receive(
			scenario.FilterByRequestID(2),
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.ErrorResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				require.Equal(t, hagallpb.ErrorCode_ERROR_CODE_NOT_FOUND, res.Code)
				return err
			}).
		Run(ctx)
	require.NoError(t, err)
}

func TestHandleAssetInstanceAddNotOwnedEntity(t *testing.T) {
	clientA, clientB, close := NewTestingEnv(t, newTestHandler(newOdalTestModule))
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
				RequestId: 3,
				SessionId: sessionID,
			}
		}).
		Receive(
			scenario.FilterByRequestID(3),
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_PARTICIPANT_JOIN_RESPONSE),
		).
		Send(func() hwebsocket.ProtoMsg {
			return &odalpb.AssetInstanceAddRequest{
				Type:      odalpb.MsgType_MSG_TYPE_ODAL_ASSET_INSTANCE_ADD_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 4,
				EntityId:  entityID,
				AssetId:   "Ted's Flipflop",
			}
		}).
		Receive(
			scenario.FilterByRequestID(4),
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ERROR_RESPONSE),
			func(msg hwebsocket.Msg) error {
				var res hagallpb.ErrorResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				require.Equal(t, hagallpb.ErrorCode_ERROR_CODE_UNAUTHORIZED, res.Code)
				return err
			}).
		Run(ctx)
	require.NoError(t, err)
}

func TestOdalHandleEntityDelete(t *testing.T) {
	clientA, clientB, close := NewTestingEnv(t, newTestHandler(newOdalTestModule))
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
			return &odalpb.AssetInstanceAddRequest{
				Type:      odalpb.MsgType_MSG_TYPE_ODAL_ASSET_INSTANCE_ADD_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 3,
				AssetId:   "Ted's nunchaku",
				EntityId:  entityID,
			}
		}).
		Receive(
			scenario.FilterByRequestID(3),
			scenario.FilterByType(odalpb.MsgType_MSG_TYPE_ODAL_ASSET_INSTANCE_ADD_RESPONSE),
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
			scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_DELETE_RESPONSE),
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
			scenario.FilterByType(odalpb.MsgType_MSG_TYPE_ODAL_STATE),
			func(msg hwebsocket.Msg) error {
				var s odalpb.State
				err := msg.DataTo(&s)
				require.NoError(t, err)

				require.Empty(t, s.AssetInstances)
				return nil
			},
		).
		Run(ctx)
	require.NoError(t, err)
}

func TestOdalHandleDisconnect(t *testing.T) {
	t.Run("disconnection", func(t *testing.T) {

		clientA, clientB, close := NewTestingEnv(t, newTestHandler(newOdalTestModule))
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
				return &odalpb.AssetInstanceAddRequest{
					Type:      odalpb.MsgType_MSG_TYPE_ODAL_ASSET_INSTANCE_ADD_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 4,
					EntityId:  entityID,
					AssetId:   "Ted's pan",
				}
			}).
			Receive(
				scenario.FilterByRequestID(4),
				scenario.FilterByType(odalpb.MsgType_MSG_TYPE_ODAL_ASSET_INSTANCE_ADD_RESPONSE),
			).
			Send(func() hwebsocket.ProtoMsg {
				return &hagallpb.EntityAddRequest{
					Type:      hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 5,
					Persist:   true,
				}
			}).
			Receive(
				scenario.FilterByRequestID(5),
				scenario.FilterByType(hagallpb.MsgType_MSG_TYPE_ENTITY_ADD_RESPONSE),
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

	t.Run("cleanup entity's assets", func(t *testing.T) {
		clientA, clientB, close := NewTestingEnv(t, newTestHandler(newOdalTestModule))
		defer close()

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
		defer cancel()

		var sessionID string
		var entityID uint32

		// clientA join & add entity assets
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
				return &odalpb.AssetInstanceAddRequest{
					Type:      odalpb.MsgType_MSG_TYPE_ODAL_ASSET_INSTANCE_ADD_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 4,
					EntityId:  entityID,
					AssetId:   "Ted's pan",
				}
			}).
			Receive(
				scenario.FilterByRequestID(4),
				scenario.FilterByType(odalpb.MsgType_MSG_TYPE_ODAL_ASSET_INSTANCE_ADD_RESPONSE),
			).
			Run(ctx)
		require.NoError(t, err)

		// clientB join & received entity assets
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
			Receive(scenario.FilterByType(odalpb.MsgType_MSG_TYPE_ODAL_STATE),
				func(msg hwebsocket.Msg) error {
					var res odalpb.State
					err := msg.DataTo(&res)
					require.NoError(t, err)

					require.Equal(t, 1, len(res.AssetInstances))
					require.Equal(t, entityID, res.AssetInstances[0].EntityId)
					require.Equal(t, "Ted's pan", res.AssetInstances[0].AssetId)
					return nil
				},
			).
			Run(ctx)
		require.NoError(t, err)

		// clientA left
		clientA.Close()

		// clientB re-join & entity assets has been cleaned up
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
			Receive(scenario.FilterByType(odalpb.MsgType_MSG_TYPE_ODAL_STATE),
				func(msg hwebsocket.Msg) error {
					var res odalpb.State
					err := msg.DataTo(&res)
					require.NoError(t, err)

					require.Equal(t, 0, len(res.AssetInstances))
					return nil
				},
			).
			Run(ctx)
		require.NoError(t, err)

	})

	t.Run("keep persistent entity's assets", func(t *testing.T) {
		clientA, clientB, close := NewTestingEnv(t, newTestHandler(newOdalTestModule))
		defer close()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		var sessionID string
		var entityID uint32

		// clientA join & add entity assets
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
				return &odalpb.AssetInstanceAddRequest{
					Type:      odalpb.MsgType_MSG_TYPE_ODAL_ASSET_INSTANCE_ADD_REQUEST,
					Timestamp: timestamppb.Now(),
					RequestId: 4,
					EntityId:  entityID,
					AssetId:   "Ted's pan",
				}
			}).
			Receive(
				scenario.FilterByRequestID(4),
				scenario.FilterByType(odalpb.MsgType_MSG_TYPE_ODAL_ASSET_INSTANCE_ADD_RESPONSE),
			).
			Run(ctx)
		require.NoError(t, err)

		// clientB join & received entity assets
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
			Receive(scenario.FilterByType(odalpb.MsgType_MSG_TYPE_ODAL_STATE),
				func(msg hwebsocket.Msg) error {
					var res odalpb.State
					err := msg.DataTo(&res)
					require.NoError(t, err)

					require.Equal(t, 1, len(res.AssetInstances))
					require.Equal(t, entityID, res.AssetInstances[0].EntityId)
					require.Equal(t, "Ted's pan", res.AssetInstances[0].AssetId)
					return nil
				},
			).
			Run(ctx)
		require.NoError(t, err)

		// clientA left
		clientA.Close()

		// clientB re-join & and receive persistent entity assets
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
			Receive(scenario.FilterByType(odalpb.MsgType_MSG_TYPE_ODAL_STATE),
				func(msg hwebsocket.Msg) error {
					var res odalpb.State
					err := msg.DataTo(&res)
					require.NoError(t, err)

					require.Equal(t, 1, len(res.AssetInstances))
					require.Equal(t, entityID, res.AssetInstances[0].EntityId)
					require.Equal(t, "Ted's pan", res.AssetInstances[0].AssetId)
					return nil
				},
			).
			Run(ctx)
		require.NoError(t, err)
	})
}

func newOdalTestModule() modules.Module {
	return &odal.Module{}
}
