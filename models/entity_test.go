package models

import (
	"testing"

	"github.com/aukilabs/hagall-common/errors"
	"github.com/aukilabs/hagall-common/messages/hagallpb"
	hwebsocket "github.com/aukilabs/hagall-common/websocket"
	"github.com/stretchr/testify/require"
)

func TestEntityPose(t *testing.T) {
	var e Entity

	p := Pose{
		PX: 1.0,
		PY: 2.0,
		PZ: 3.0,
		RX: 4.0,
		RY: 5.0,
		RZ: 6.0,
		RW: 7.0,
	}

	e.SetPose(p)
	require.Equal(t, p, e.Pose())
}

func TestEntityToProtobuf(t *testing.T) {
	e := Entity{
		ID:            1,
		ParticipantID: 11,
		pose: Pose{
			PX: 1.0,
			PY: 2.0,
			PZ: 3.0,
			RX: 4.0,
			RY: 5.0,
			RZ: 6.0,
			RW: 7.0,
		},
		Flag: hagallpb.EntityFlag_ENTITY_FLAG_PARTICIPANT_ENTITY,
	}

	pe := e.ToProtobuf()
	require.Equal(t, e.ID, pe.Id)
	require.Equal(t, e.ParticipantID, pe.ParticipantId)

	require.Equal(t, e.pose.PX, pe.Pose.Px)
	require.Equal(t, e.pose.PY, pe.Pose.Py)
	require.Equal(t, e.pose.PZ, pe.Pose.Pz)
	require.Equal(t, e.pose.RX, pe.Pose.Rx)
	require.Equal(t, e.pose.RY, pe.Pose.Ry)
	require.Equal(t, e.pose.RZ, pe.Pose.Rz)
	require.Equal(t, e.pose.RW, pe.Pose.Rw)
	require.Equal(t, e.Flag, pe.Flag)
}

func TestEntitiesToProtobuf(t *testing.T) {
	e := &Entity{
		ID:            1,
		ParticipantID: 11,
		pose: Pose{
			PX: 1.0,
			PY: 2.0,
			PZ: 3.0,
			RX: 4.0,
			RY: 5.0,
			RZ: 6.0,
			RW: 7.0,
		},
	}

	pEntities := EntitiesToProtobuf([]*Entity{e})
	require.Len(t, pEntities, 1)

	pe := pEntities[0]
	require.Equal(t, e.ID, pe.Id)
	require.Equal(t, e.ParticipantID, pe.ParticipantId)

	require.Equal(t, e.pose.PX, pe.Pose.Px)
	require.Equal(t, e.pose.PY, pe.Pose.Py)
	require.Equal(t, e.pose.PZ, pe.Pose.Pz)
	require.Equal(t, e.pose.RX, pe.Pose.Rx)
	require.Equal(t, e.pose.RY, pe.Pose.Ry)
	require.Equal(t, e.pose.RZ, pe.Pose.Rz)
	require.Equal(t, e.pose.RW, pe.Pose.Rw)
}

func TestPoseToProtobuf(t *testing.T) {
	p := Pose{
		PX: 1.0,
		PY: 2.0,
		PZ: 3.0,
		RX: 4.0,
		RY: 5.0,
		RZ: 6.0,
		RW: 7.0,
	}

	pp := p.ToProtobuf()
	require.Equal(t, p.PX, pp.Px)
	require.Equal(t, p.PY, pp.Py)
	require.Equal(t, p.PZ, pp.Pz)
	require.Equal(t, p.RX, pp.Rx)
	require.Equal(t, p.RY, pp.Ry)
	require.Equal(t, p.RZ, pp.Rz)
	require.Equal(t, p.RW, pp.Rw)
}

func TestEntityComponentStoreAddType(t *testing.T) {
	t.Run("entity component type is registered", func(t *testing.T) {
		s := newEntityComponentStore()
		ectID := s.AddType("hello")
		require.NotZero(t, ectID)
	})

	t.Run("adding added entity component type returns an error", func(t *testing.T) {
		s := newEntityComponentStore()
		ectID := s.AddType("hello")
		require.Equal(t, ectID, s.AddType("hello"))
	})
}

func TestEntityComponentStoreGetTypeName(t *testing.T) {
	t.Run("getting name of added entity component type succeed", func(t *testing.T) {
		s := newEntityComponentStore()
		ectID := s.AddType("foo")

		name, err := s.GetTypeName(ectID)
		require.NoError(t, err)
		require.Equal(t, "foo", name)
	})

	t.Run("getting name of not added entity component type returns an error", func(t *testing.T) {
		s := newEntityComponentStore()
		name, err := s.GetTypeName(42)
		require.Error(t, err)
		require.Zero(t, name)
	})
}

func TestEntityComponentStoreGetTypeID(t *testing.T) {
	t.Run("getting id of add entity component type succeed", func(t *testing.T) {
		s := newEntityComponentStore()
		ectID := s.AddType("foo")

		gID, err := s.GetTypeID("foo")
		require.NoError(t, err)
		require.Equal(t, ectID, gID)
	})

	t.Run("getting id of not added entity component type returns an error", func(t *testing.T) {
		s := newEntityComponentStore()
		ectID, err := s.GetTypeID("foo")
		require.Error(t, err)
		require.Zero(t, ectID)
	})
}

func TestEntityComponentStoreAdd(t *testing.T) {
	t.Run("adding a not registered entity component returns an error", func(t *testing.T) {
		s := newEntityComponentStore()
		err := s.Add(&hagallpb.EntityComponent{
			EntityComponentTypeId: 42,
			EntityId:              21,
		})
		require.Error(t, err)
		require.Equal(t, hwebsocket.ErrEntityComponentTypeNotAdded, errors.Type(err))
	})

	t.Run("adding registered entity component succeeds", func(t *testing.T) {
		s := newEntityComponentStore()
		ectID := s.AddType("foo")

		ec := &hagallpb.EntityComponent{
			EntityComponentTypeId: ectID,
			EntityId:              21,
		}

		err := s.Add(ec)
		require.NoError(t, err)
		require.Len(t, s.entityComponents, 1)
		require.Len(t, s.entityComponents[ectID], 1)
		require.Equal(t, ec, s.entityComponents[ec.EntityComponentTypeId][ec.EntityId])
	})

	t.Run("adding an already added entity component returns an error", func(t *testing.T) {
		s := newEntityComponentStore()
		ectID := s.AddType("foo")

		ec := &hagallpb.EntityComponent{
			EntityComponentTypeId: ectID,
			EntityId:              21,
		}

		err := s.Add(ec)
		require.NoError(t, err)

		err = s.Add(ec)
		require.Error(t, err)
		require.Equal(t, hwebsocket.ErrEntityComponentTypeAlreadyAdded, errors.Type(err))
	})
}

func TestEntityComponentStoreDelete(t *testing.T) {
	t.Run("nonexistent entity component is removed", func(t *testing.T) {
		s := newEntityComponentStore()
		require.False(t, s.Delete(42, 21))
	})

	t.Run("entity component is removed", func(t *testing.T) {
		s := newEntityComponentStore()
		ectID := s.AddType("foo")

		err := s.Add(&hagallpb.EntityComponent{
			EntityComponentTypeId: ectID,
			EntityId:              21,
		})
		require.NoError(t, err)

		require.True(t, s.Delete(ectID, 21))
		require.Len(t, s.entityComponents, 1)
		require.Empty(t, s.entityComponents[ectID])
	})
}

func TestEntityComponentStoreDeleteByEntityID(t *testing.T) {
	s := newEntityComponentStore()
	ectID := s.AddType("foo")

	err := s.Add(&hagallpb.EntityComponent{
		EntityComponentTypeId: ectID,
		EntityId:              21,
	})
	require.NoError(t, err)

	err = s.Add(&hagallpb.EntityComponent{
		EntityComponentTypeId: ectID,
		EntityId:              22,
	})
	require.NoError(t, err)

	s.DeleteByEntityID(21)
	require.Len(t, s.entityComponents, 1)
	require.Len(t, s.entityComponents[ectID], 1)
}

func TestEntityComponentStoreUpdate(t *testing.T) {
	t.Run("updating a nonexisting entity component returns an error", func(t *testing.T) {
		s := newEntityComponentStore()
		err := s.Update(&hagallpb.EntityComponent{
			EntityComponentTypeId: 42,
			EntityId:              21,
		})
		require.Error(t, err)
	})

	t.Run("updating a removed entity component returns an error", func(t *testing.T) {
		s := newEntityComponentStore()
		eaID := s.AddType("foo")

		err := s.Add(&hagallpb.EntityComponent{
			EntityComponentTypeId: eaID,
			EntityId:              21,
		})
		require.NoError(t, err)

		s.Delete(eaID, 21)
		err = s.Update(&hagallpb.EntityComponent{
			EntityComponentTypeId: eaID,
			EntityId:              21,
		})
		require.Error(t, err)
	})

	t.Run("updating an entity component succeeds", func(t *testing.T) {
		s := newEntityComponentStore()
		eaID := s.AddType("foo")

		ea := &hagallpb.EntityComponent{
			EntityComponentTypeId: eaID,
			EntityId:              21,
			Data:                  []byte("1"),
		}

		err := s.Add(ea)
		require.NoError(t, err)

		err = s.Update(ea)
		require.NoError(t, err)
		require.Equal(t, ea, s.entityComponents[ea.EntityComponentTypeId][ea.EntityId])
	})
}

func TestEntityComponentStoreList(t *testing.T) {
	t.Run("list non added entity component returns nil", func(t *testing.T) {
		s := newEntityComponentStore()
		l := s.List(42)
		require.Nil(t, l)
	})

	t.Run("list enttity components succeeds", func(t *testing.T) {
		s := newEntityComponentStore()
		ectID := s.AddType("foo")

		s.Add(&hagallpb.EntityComponent{EntityComponentTypeId: ectID, EntityId: 42})
		s.Add(&hagallpb.EntityComponent{EntityComponentTypeId: ectID, EntityId: 41})
		s.Add(&hagallpb.EntityComponent{EntityComponentTypeId: ectID, EntityId: 43})

		l := s.List(ectID)
		require.Len(t, l, 3)
	})
}

func TestEntityComponentStoreListByEntityID(t *testing.T) {
	t.Run("list non added entity component returns nil", func(t *testing.T) {
		s := newEntityComponentStore()
		l := s.ListByEntityID(42)
		require.Nil(t, l)
	})

	t.Run("list enttity components succeeds", func(t *testing.T) {
		s := newEntityComponentStore()
		fooID := s.AddType("foo")
		barID := s.AddType("bar")

		s.Add(&hagallpb.EntityComponent{EntityComponentTypeId: fooID, EntityId: 42})
		s.Add(&hagallpb.EntityComponent{EntityComponentTypeId: fooID, EntityId: 41})
		s.Add(&hagallpb.EntityComponent{EntityComponentTypeId: barID, EntityId: 42})

		l := s.ListByEntityID(42)
		require.Len(t, l, 2)
	})
}

func TestEntityComponentStoreListAll(t *testing.T) {
	s := newEntityComponentStore()
	fooID := s.AddType("foo")
	barID := s.AddType("bar")

	s.Add(&hagallpb.EntityComponent{EntityComponentTypeId: fooID, EntityId: 42})
	s.Add(&hagallpb.EntityComponent{EntityComponentTypeId: fooID, EntityId: 41})
	s.Add(&hagallpb.EntityComponent{EntityComponentTypeId: barID, EntityId: 42})

	l := s.ListAll()
	require.Len(t, l, 3)
}

func TestEntityComponentStoreSubscribe(t *testing.T) {
	t.Run("subscribing to unregistered entity component returns an error", func(t *testing.T) {
		s := newEntityComponentStore()

		err := s.Subscribe(42, 88)
		require.Error(t, err)
	})

	t.Run("subscribing to registered entity succeeds", func(t *testing.T) {
		s := newEntityComponentStore()
		eaID := s.AddType("foo")

		err := s.Subscribe(eaID, 88)
		require.NoError(t, err)
		require.Len(t, s.subscriptions, 1)
		require.Len(t, s.subscriptions[eaID], 1)
		require.NotNil(t, s.subscriptions[eaID][88])
	})
}

func TestEntityComponentStoreUnsubscribe(t *testing.T) {
	t.Run("unsuscribing to unregistered entity component", func(t *testing.T) {
		s := newEntityComponentStore()
		s.Unsubscribe(42, 88)
	})

	t.Run("unsuscribing to registerd entity component", func(t *testing.T) {
		s := newEntityComponentStore()
		eaID := s.AddType("foo")

		err := s.Subscribe(eaID, 88)
		require.NoError(t, err)

		s.Unsubscribe(eaID, 88)
		require.Empty(t, s.subscriptions[eaID])
	})
}

func TestEntityComponentStoreUnsubscribeByParticipant(t *testing.T) {
	t.Run("unsuscribing to unregistered entity component", func(t *testing.T) {
		s := newEntityComponentStore()
		s.UnsubscribeByParticipant(88)
	})

	t.Run("unsuscribing to registerd entity component", func(t *testing.T) {
		s := newEntityComponentStore()
		eaID := s.AddType("foo")

		err := s.Subscribe(eaID, 88)
		require.NoError(t, err)

		err = s.Subscribe(eaID, 89)
		require.NoError(t, err)

		require.Len(t, s.subscriptions[eaID], 2)

		s.UnsubscribeByParticipant(88)
		require.Len(t, s.subscriptions[eaID], 1)
		require.NotNil(t, s.subscriptions[eaID][89])
	})
}

func TestEntityComponentStoreNotify(t *testing.T) {
	t.Run("notify handler without subscription is not called", func(t *testing.T) {
		s := newEntityComponentStore()

		isHandlerCalled := false
		s.Notify(42, func(u []uint32) {
			isHandlerCalled = true
		})
		require.False(t, isHandlerCalled)
	})

	t.Run("notify handler is called", func(t *testing.T) {
		s := newEntityComponentStore()
		ectID := s.AddType("hello")

		err := s.Subscribe(ectID, 88)
		require.NoError(t, err)

		isHandlerCalled := false
		s.Notify(ectID, func(paritcipantIDs []uint32) {
			isHandlerCalled = true
			require.Len(t, paritcipantIDs, 1)
			require.Equal(t, uint32(88), paritcipantIDs[0])
		})
		require.True(t, isHandlerCalled)
	})
}
