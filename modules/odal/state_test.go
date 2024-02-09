package odal

import (
	"testing"

	"github.com/aukilabs/hagall-common/messages/odalpb"
	"github.com/stretchr/testify/require"
)

func TestStateAssetInstance(t *testing.T) {
	t.Run("asset instance is retrieved", func(t *testing.T) {
		var s State

		aiA := &odalpb.AssetInstance{
			Id:            s.NewAssetInstanceID(),
			AssetId:       "ted's nunchaku",
			ParticipantId: 42,
			EntityId:      21,
		}
		s.SetAssetInstance(aiA)
		require.Len(t, s.assetInstances, 1)

		aiB, ok := s.AssetInstance(aiA.EntityId)
		require.True(t, ok)
		require.Equal(t, aiA, aiB)
	})

	t.Run("asset instance is not found", func(t *testing.T) {
		var s State

		ai, ok := s.AssetInstance(21)
		require.False(t, ok)
		require.Nil(t, ai)
	})
}

func TestStateRemoveAssetInstance(t *testing.T) {
	var s State

	ai := &odalpb.AssetInstance{
		Id:            s.NewAssetInstanceID(),
		AssetId:       "ted's nunchaku",
		ParticipantId: 42,
		EntityId:      21,
	}
	s.SetAssetInstance(ai)
	require.Len(t, s.assetInstances, 1)

	s.RemoveAssetInstance(ai.EntityId)
	require.Empty(t, s.assetInstances)
}

func TestStateAssetInstances(t *testing.T) {
	var s State

	ai := &odalpb.AssetInstance{
		Id:            s.NewAssetInstanceID(),
		AssetId:       "ted's nunchaku",
		ParticipantId: 42,
		EntityId:      21,
	}
	s.SetAssetInstance(ai)

	ais := s.AssetInstances()
	require.Len(t, ais, 1)
	require.Equal(t, ai, ais[0])
}
