package models

import (
	"encoding/base64"
	"errors"
	"github.com/google/uuid"
	"strings"
)

const CircuitRelayCapabilityName = "/p2p/circuit-relay/v1"

type DomainServerCredentials struct {
	ID     string `json:"id"`
	Secret string `json:"secret"`
}

func ValidateRegistrationCredentials(credentials string) (*DomainServerCredentials, error) {
	str, err := base64.StdEncoding.DecodeString(credentials)
	if err != nil {
		return nil, errors.New("invalid registration credentials encoding")
	}

	split := strings.Split(string(str), ":")
	if len(split) != 2 {
		return nil, errors.New("invalid registration credentials format")
	}

	_, err = uuid.Parse(split[0])
	if err != nil {
		return nil, errors.New("invalid uuid in registration credentials")
	}
	return &DomainServerCredentials{
		ID:     split[0],
		Secret: split[1],
	}, nil
}
