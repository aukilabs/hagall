package authpkg

import (
	"github.com/aukilabs/go-tooling/pkg/errors"
	"github.com/golang-jwt/jwt/v4"
	"github.com/libp2p/go-libp2p/core/peer"
	"strings"
)

func validateOptionalMachinePeerID(peerID *string) error {
	if peerID == nil {
		return nil
	}
	if err := validateMachinePeerID(*peerID); err != nil {
		return jwt.NewValidationError("peer_id must be a valid libp2p Peer ID", jwt.ValidationErrorClaimsInvalid)
	}
	return nil
}

func validateMachinePeerID(peerID string) error {
	if strings.TrimSpace(peerID) == "" {
		return errors.New("peer_id is required")
	}
	if _, err := peer.Decode(peerID); err != nil {
		return errors.New("peer_id must be a valid libp2p Peer ID").Wrap(err)
	}
	return nil
}
