package models

import (
	"github.com/aukilabs/hagall-common/messages/hagallpb"
	hwebsocket "github.com/aukilabs/hagall-common/websocket"
)

// A session participant.
type Participant struct {
	ID        uint32
	Responder hwebsocket.ResponseSender

	entityIDs map[uint32]struct{}
}

func (p *Participant) AddEntity(e *Entity) {
	if p.entityIDs == nil {
		p.entityIDs = make(map[uint32]struct{})
	}
	p.entityIDs[e.ID] = struct{}{}
}

func (p *Participant) RemoveEntity(e *Entity) {
	delete(p.entityIDs, e.ID)
}

func (p *Participant) EntityIDs() map[uint32]struct{} {
	return p.entityIDs
}

func (p *Participant) ToProtobuf() *hagallpb.Participant {
	return &hagallpb.Participant{
		Id: p.ID,
	}
}

func ParticipantsToProtobuf(participants []*Participant) []*hagallpb.Participant {
	res := make([]*hagallpb.Participant, len(participants))
	for i, p := range participants {
		res[i] = p.ToProtobuf()
	}
	return res
}
