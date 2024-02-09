package models

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParticipantAddEntity(t *testing.T) {
	p := Participant{
		ID: 1,
	}

	e := &Entity{
		ID:            1,
		ParticipantID: 1,
	}

	p.AddEntity(e)
	require.Len(t, p.EntityIDs(), 1)
}

func TestParticipantRemoveEntity(t *testing.T) {
	p := Participant{
		ID: 1,
	}

	e := &Entity{
		ID:            1,
		ParticipantID: 1,
	}

	p.AddEntity(e)
	require.Len(t, p.EntityIDs(), 1)

	p.RemoveEntity(e)
	require.Empty(t, p.EntityIDs())
}

func TestParticipantToProtobuf(t *testing.T) {
	p := Participant{
		ID: 1,
	}

	pp := p.ToProtobuf()
	require.Equal(t, p.ID, pp.Id)
}

func TestParticipantsToProtobuf(t *testing.T) {
	participants := []*Participant{
		{
			ID: 1,
		},
		{
			ID: 2,
		},
	}

	protoParticipants := ParticipantsToProtobuf(participants)
	require.Len(t, protoParticipants, 2)
}
