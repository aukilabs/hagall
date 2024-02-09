package vikja

import (
	"testing"

	"github.com/aukilabs/hagall-common/messages/vikjapb"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestStateSetEntityAction(t *testing.T) {
	var s State

	eaA := &vikjapb.EntityAction{
		Name:      "Ted's Tornado kick",
		EntityId:  42,
		Timestamp: timestamppb.Now(),
	}

	eaB := &vikjapb.EntityAction{
		Name:      "Ted's Tiger Claws",
		EntityId:  42,
		Timestamp: timestamppb.Now(),
	}

	s.SetEntityAction(eaA)
	s.SetEntityAction(eaB)
	require.Len(t, s.entityActions, 1)
	require.Len(t, s.entityActions[42], 2)
	require.Equal(t, eaA, s.entityActions[42][eaA.Name])
	require.Equal(t, eaB, s.entityActions[42][eaB.Name])
}

func TestStateEntityAction(t *testing.T) {
	t.Run("entity action is retrieved", func(t *testing.T) {
		var s State

		eaA := &vikjapb.EntityAction{
			Name:      "Ted's Tornado Kick",
			EntityId:  42,
			Timestamp: timestamppb.Now(),
			Data:      []byte("with kime"),
		}
		s.SetEntityAction(eaA)

		eaB, ok := s.EntityAction(eaA.EntityId, eaA.Name)
		require.True(t, ok)
		require.Equal(t, eaA, eaB)

	})

	t.Run("entity action with bad entity id is not found", func(t *testing.T) {
		var s State

		ea, ok := s.EntityAction(42, "Ted's Tiger Claws")
		require.False(t, ok)
		require.Nil(t, ea)
	})

	t.Run("entity action is retrieved", func(t *testing.T) {
		var s State

		eaA := &vikjapb.EntityAction{
			Name:      "Ted's Tornado Kick",
			EntityId:  42,
			Timestamp: timestamppb.Now(),
			Data:      []byte("with kime"),
		}
		s.SetEntityAction(eaA)

		eaB, ok := s.EntityAction(eaA.EntityId, "Ted's Tiger Claws")
		require.False(t, ok)
		require.Nil(t, eaB)
	})
}

func TestStateRemoveEntityAction(t *testing.T) {
	var s State

	s.SetEntityAction(&vikjapb.EntityAction{
		Name:      "Ted's Tornado kick",
		EntityId:  42,
		Timestamp: timestamppb.Now(),
	})
	s.SetEntityAction(&vikjapb.EntityAction{
		Name:      "Ted's Tiger Claws",
		EntityId:  42,
		Timestamp: timestamppb.Now(),
	})
	require.Len(t, s.entityActions, 1)

	s.RemoveEntityActions(42)
	require.Empty(t, s.entityActions)
}

func TestStateEntityActions(t *testing.T) {
	var s State

	ea := &vikjapb.EntityAction{
		Name:      "Ted's Tiger Claws",
		EntityId:  42,
		Timestamp: timestamppb.Now(),
	}
	s.SetEntityAction(ea)

	eas := s.EntityActions()
	require.Len(t, eas, 1)
	require.Equal(t, ea, eas[0])
}
