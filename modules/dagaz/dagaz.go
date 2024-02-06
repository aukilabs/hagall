package dagaz

import (
	"context"

	"github.com/aukilabs/go-tooling/pkg/errors"
	"github.com/aukilabs/hagall-common/messages/dagazpb"
	hwebsocket "github.com/aukilabs/hagall-common/websocket"
	"github.com/aukilabs/hagall/models"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TODO(jhenriques): Can we remove timestamps from protobuf messages? Dagaz does not use them...

type Module struct {
	currentSession     *models.Session
	currentParticipant *models.Participant
	state              *State
}

func (m *Module) Name() string {
	return "dagaz"
}

func (m *Module) Init(s *models.Session, p *models.Participant) {
	m.currentSession = s
	m.currentParticipant = p

	state, ok := s.ModuleState(m.Name())
	if !ok {
		state = &State{}
		s.SetModuleState(m.Name(), state)
	}
	m.state = state.(*State)

	m.state.SpatialPartition = NewRegularGrid(1, 1, 2)
}

func (m *Module) HandleMsg(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error {
	var err error

	switch dagazpb.MsgType(msg.Type.Number()) {
	case dagazpb.MsgType_MSG_TYPE_DAGAZ_QUAD_SAMPLE:
		err = m.HandleDagazQuadSample(ctx, msg)

	case dagazpb.MsgType_MSG_TYPE_DAGAZ_GET_GROUND_PLANE_REQUEST:
		err = m.HandleDagazGetGroundPlane(ctx, respond, msg)

	case dagazpb.MsgType_MSG_TYPE_DAGAZ_GET_REGION_REQUEST:
		err = m.HandleDagazGetRegion(ctx, respond, msg)

	case dagazpb.MsgType_MSG_TYPE_DAGAZ_GET_DEBUG_INFO_REQUEST:
		err = m.HandleDagazGetDebugInfo(ctx, respond, msg)
	}

	return err
}

func (m *Module) HandleDisconnect() {
}

func (m *Module) HandleDagazQuadSample(ctx context.Context, msg hwebsocket.Msg) error {
	var newQuadSample dagazpb.DagazQuadSample
	if err := msg.DataTo(&newQuadSample); err != nil {
		return err
	}

	session := m.currentSession
	if session == nil {
		return errors.New("session not joined").
			WithType(hwebsocket.ErrTypeSessionNotJoined).
			WithTag("msg_type", msg.Type)
	}

	for _, newQuad := range newQuadSample.Samples {
		quad := NewQuadFromProtobuf(newQuad)
		m.state.SpatialPartition.InsertQuad(quad)
	}

	return nil
}

func (m *Module) HandleDagazGetGroundPlane(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error {
	var req dagazpb.DagazGetGroundPlaneRequest
	if err := msg.DataTo(&req); err != nil {
		return err
	}

	session := m.currentSession
	if session == nil {
		return errors.New("session not joined").
			WithType(hwebsocket.ErrTypeSessionNotJoined).
			WithTag("msg_type", msg.Type)
	}

	ray := NewRayFromProtobuf(req.Ray)
	quadHit, _ := m.state.SpatialPartition.IntersectQuad(ray)

	if quadHit == nil {
		// create an invalid quad to be able to have a response:
		quadHit = &Quad{
			Center:  Vector3f{0, 0, 0},
			Extents: Vector3f{0, 0, 0},
			Normal:  Vector3f{0, 0, 0},
		}
	}
	sampleGroundQuad := quadHit.ToProtobuf()

	respond.Send(&dagazpb.DagazGetGroundPlaneResponse{
		Type:      dagazpb.MsgType_MSG_TYPE_DAGAZ_GET_GROUND_PLANE_RESPONSE,
		Timestamp: timestamppb.Now(),
		RequestId: req.RequestId,
		Ground:    sampleGroundQuad,
	})
	return nil
}

func (m *Module) HandleDagazGetRegion(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error {
	var req dagazpb.DagazGetRegionRequest
	if err := msg.DataTo(&req); err != nil {
		return err
	}

	session := m.currentSession
	if session == nil {
		return errors.New("session not joined").
			WithType(hwebsocket.ErrTypeSessionNotJoined).
			WithTag("msg_type", msg.Type)
	}

	regionQuads := m.state.SpatialPartition.GetRegion(NewVector3fFromProtobuf(req.Min), NewVector3fFromProtobuf(req.Max))
	regionQuadsProtobuf := make([]*dagazpb.Quad, len(regionQuads))
	for i := 0; i < len(regionQuads); i++ {
		regionQuadsProtobuf[i] = regionQuads[i].ToProtobuf()
	}

	respond.Send(&dagazpb.DagazGetRegionResponse{
		Type:      dagazpb.MsgType_MSG_TYPE_DAGAZ_GET_REGION_RESPONSE,
		Timestamp: timestamppb.Now(),
		RequestId: req.RequestId,
		Quads:     regionQuadsProtobuf,
	})
	return nil
}

func (m *Module) HandleDagazGetDebugInfo(ctx context.Context, respond hwebsocket.ResponseSender, msg hwebsocket.Msg) error {
	var req dagazpb.DagazGetDebugInfoRequest
	if err := msg.DataTo(&req); err != nil {
		return err
	}

	session := m.currentSession
	if session == nil {
		return errors.New("session not joined").
			WithType(hwebsocket.ErrTypeSessionNotJoined).
			WithTag("msg_type", msg.Type)
	}

	debugInfo := m.state.SpatialPartition.GetDebugInfo()

	respond.Send(&dagazpb.DagazGetDebugInfoResponse{
		Type:           dagazpb.MsgType_MSG_TYPE_DAGAZ_GET_DEBUG_INFO_RESPONSE,
		Timestamp:      nil,
		RequestId:      req.RequestId,
		GridResolution: debugInfo.Resolution,
		GridRowCount:   debugInfo.Row_count,
		GridColCount:   debugInfo.Col_count,
		GridPlaneCount: debugInfo.Plane_count,
		GridMergeCount: debugInfo.Merge_count,
		GridMinPoint:   debugInfo.Min_point.ToProtobuf(),
		GridMaxPoint:   debugInfo.Max_point.ToProtobuf(),
		Occupancy:      debugInfo.Occupancy,
	})
	return nil
}
