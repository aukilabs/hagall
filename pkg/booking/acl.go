package booking

import (
	"github.com/aukilabs/hagall/pkg/telemetry"
	"github.com/libp2p/go-libp2p/core/peer"
	relayv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	ma "github.com/multiformats/go-multiaddr"
)

// ACL is a dynamic, local-only Circuit Relay v2 filter backed by Registry.
// Both methods are intentionally constant-time because go-libp2p invokes them
// synchronously on concurrent HOP streams.
type ACL struct {
	registry *Registry
	observer interface {
		ObserveACL(telemetry.ACLAction, bool)
	}
}

func NewACL(registry *Registry, observers ...interface {
	ObserveACL(telemetry.ACLAction, bool)
}) *ACL {
	acl := &ACL{registry: registry}
	if len(observers) != 0 {
		acl.observer = observers[0]
	}
	return acl
}

func (a *ACL) AllowReserve(target peer.ID, _ ma.Multiaddr) bool {
	allowed := a != nil && a.registry != nil && a.registry.AllowReserve(target)
	if a != nil && a.observer != nil {
		a.observer.ObserveACL(telemetry.ACLReserve, allowed)
	}
	return allowed
}

func (a *ACL) AllowConnect(source peer.ID, _ ma.Multiaddr, target peer.ID) bool {
	allowed := a != nil && a.registry != nil && a.registry.AllowConnect(source, target)
	if a != nil && a.observer != nil {
		a.observer.ObserveACL(telemetry.ACLConnect, allowed)
	}
	return allowed
}

var _ relayv2.ACLFilter = (*ACL)(nil)
