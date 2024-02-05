package websocket

import (
	"context"
	"testing"
	"time"

	"github.com/aukilabs/hagall-common/messages/dagazpb"
	"github.com/aukilabs/hagall-common/messages/hagallpb"
	"github.com/aukilabs/hagall-common/scenario"
	hwebsocket "github.com/aukilabs/hagall-common/websocket"
	"github.com/aukilabs/hagall/modules"
	"github.com/aukilabs/hagall/modules/dagaz"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestHandleDagazQuadSample(t *testing.T) {
	clientA, _, close := NewTestingEnv(t, newTestHandler(newDagazTestModule))
	defer close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := scenario.NewScenario(clientA).
		Send(func() hwebsocket.ProtoMsg {
			return &dagazpb.DagazQuadSample{
				Type:      dagazpb.MsgType_MSG_TYPE_DAGAZ_QUAD_SAMPLE,
				Timestamp: timestamppb.Now(),
				Samples:   []*dagazpb.Quad{},
			}
		}).
		Run(ctx)
	require.NoError(t, err)
}

func TestHandleDagazGetGroundPlane(t *testing.T) {
	clientA, _, close := NewTestingEnv(t, newTestHandler(newDagazTestModule))
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

			from := dagaz.NewVector3f(0, 1, 0)
			to := dagaz.NewVector3f(0, -1, 0)

			return &dagazpb.DagazGetGroundPlaneRequest{
				Type:      dagazpb.MsgType_MSG_TYPE_DAGAZ_GET_GROUND_PLANE_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 2,
				Ray: &dagazpb.Ray{
					From: from.ToProtobuf(),
					To:   to.ToProtobuf(),
				},
			}
		}).
		Receive(
			scenario.FilterByType(dagazpb.MsgType_MSG_TYPE_DAGAZ_GET_GROUND_PLANE_RESPONSE),
			func(msg hwebsocket.Msg) error {
				var res dagazpb.DagazGetGroundPlaneResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				center := dagaz.NewVector3fFromProtobuf(res.Ground.Center)
				extents := dagaz.NewVector3fFromProtobuf(res.Ground.Extents)
				require.True(t, center.Equal(dagaz.NewVector3f(0, 0, 0)))
				require.True(t, extents.Equal(dagaz.NewVector3f(0, 0, 0)))

				return nil
			}).
		Run(ctx)
	require.NoError(t, err)
}

func TestHandleDagazGetRegion(t *testing.T) {
	clientA, _, close := NewTestingEnv(t, newTestHandler(newDagazTestModule))
	defer close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	quad := dagaz.Quad{
		Center:     dagaz.NewVector3f(0, 0, 0),
		Extents:    dagaz.NewVector3f(1, 0, 1),
		Normal:     dagaz.NewVector3f(0, 1, 0),
		MergeCount: 0,
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
		).
		Send(func() hwebsocket.ProtoMsg {
			return &dagazpb.DagazQuadSample{
				Type:      dagazpb.MsgType_MSG_TYPE_DAGAZ_QUAD_SAMPLE,
				Timestamp: timestamppb.Now(),
				Samples:   []*dagazpb.Quad{quad.ToProtobuf()},
			}
		}).
		Send(func() hwebsocket.ProtoMsg {

			min := dagaz.NewVector3f(-10, -10, -10)
			max := dagaz.NewVector3f(10, 10, 10)

			return &dagazpb.DagazGetRegionRequest{
				Type:      dagazpb.MsgType_MSG_TYPE_DAGAZ_GET_REGION_REQUEST,
				Timestamp: timestamppb.Now(),
				RequestId: 2,
				Min:       min.ToProtobuf(),
				Max:       max.ToProtobuf(),
			}
		}).
		Receive(
			scenario.FilterByType(dagazpb.MsgType_MSG_TYPE_DAGAZ_GET_REGION_RESPONSE),
			func(msg hwebsocket.Msg) error {
				var res dagazpb.DagazGetRegionResponse
				err := msg.DataTo(&res)
				require.NoError(t, err)

				quadsCount := len(res.Quads)
				require.Equal(t, quadsCount, 1)

				quad := dagaz.NewQuadFromProtobuf(res.Quads[0])
				require.True(t, quad.Center.Equal(dagaz.NewVector3f(0, 0, 0)))
				require.True(t, quad.Extents.Equal(dagaz.NewVector3f(1, 0, 1)))

				return nil
			}).
		Run(ctx)
	require.NoError(t, err)
}

func newDagazTestModule() modules.Module {
	return &dagaz.Module{}
}
