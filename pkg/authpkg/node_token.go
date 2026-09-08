package authpkg

import (
	"strings"

	"github.com/aukilabs/hagall/pkg/models"
	"github.com/golang-jwt/jwt/v4"
	"github.com/google/uuid"
)

const (
	NodeModeDedicated = "dedicated"
	NodeModePublic    = "public"
)

type NodeAccessClaims struct {
	NodeID             string   `json:"node_id"`
	OrganizationID     string   `json:"organization_id"`
	NodeMode           string   `json:"node_mode"`
	Capabilities       []string `json:"capabilities,omitempty"`
	MaxConcurrency     *int     `json:"max_concurrency,omitempty"`
	SchedulingRevision *int64   `json:"scheduling_revision,omitempty"`
	PeerID             *string  `json:"peer_id,omitempty"`
	jwt.RegisteredClaims
}

func (c NodeAccessClaims) Valid() error {
	if c.NodeID == "" {
		return jwt.NewValidationError("node_id is required", jwt.ValidationErrorClaimsInvalid)
	}
	if _, err := uuid.Parse(c.OrganizationID); err != nil {
		return jwt.NewValidationError("organization_id must be a valid uuid", jwt.ValidationErrorClaimsInvalid)
	}
	if !isValidNodeMode(c.NodeMode) {
		return jwt.NewValidationError("node_mode must be 'public' or 'dedicated'", jwt.ValidationErrorClaimsInvalid)
	}
	if c.MaxConcurrency != nil && *c.MaxConcurrency <= 0 {
		return jwt.NewValidationError("max_concurrency must be positive", jwt.ValidationErrorClaimsInvalid)
	}
	if c.SchedulingRevision != nil && *c.SchedulingRevision < 0 {
		return jwt.NewValidationError("scheduling_revision must be non-negative", jwt.ValidationErrorClaimsInvalid)
	}
	if containsCapability(c.Capabilities, models.CircuitRelayCapabilityName) && c.SchedulingRevision == nil {
		return jwt.NewValidationError("scheduling_revision is required for circuit relay nodes", jwt.ValidationErrorClaimsInvalid)
	}
	if err := validateOptionalMachinePeerID(c.PeerID); err != nil {
		return err
	}
	return c.RegisteredClaims.Valid()
}

func containsCapability(capabilities []string, expected string) bool {
	for _, capability := range capabilities {
		if capability == expected {
			return true
		}
	}
	return false
}

func isValidNodeMode(mode string) bool {
	switch strings.ToLower(mode) {
	case NodeModeDedicated, NodeModePublic:
		return true
	default:
		return false
	}
}
