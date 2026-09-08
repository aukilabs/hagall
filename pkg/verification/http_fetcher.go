package verification

import (
	"bytes"
	"context"
	"crypto"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v4"
)

const maxPublicKeyResponseBytes = 64 << 10

const (
	verificationKeySetVersion = 1
	verificationKeyCurrent    = "current"
	verificationKeyPrevious   = "previous"
)

type HTTPFetcher struct {
	URL    string
	Client *http.Client
}

type verificationKeyResponse struct {
	ID            string `json:"id"`
	Status        string `json:"status"`
	PublicKey     string `json:"public_key"`
	SigningMethod string `json:"signing_method"`
}

type verificationKeySetResponse struct {
	Version                   uint8                     `json:"version"`
	Generation                uint64                    `json:"generation"`
	PreviousKeyOverlapSeconds uint64                    `json:"previous_key_overlap_seconds"`
	Keys                      []verificationKeyResponse `json:"keys"`
}

func (f *HTTPFetcher) Fetch(ctx context.Context) (KeySet, error) {
	if f == nil || f.Client == nil {
		return KeySet{}, errors.New("DDS verification-key HTTP client is required")
	}
	if f.URL == "" {
		return KeySet{}, errors.New("DDS verification-key URL is required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return KeySet{}, fmt.Errorf("create DDS verification-key request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	// A credential signed by a just-rotated key must force intermediaries to
	// revalidate instead of replaying a pre-rotation key set.
	req.Header.Set("Cache-Control", "no-cache")
	client := *f.Client
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	response, err := client.Do(req)
	if err != nil {
		return KeySet{}, fmt.Errorf("fetch DDS verification keys: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return KeySet{}, fmt.Errorf("fetch DDS verification keys: unexpected HTTP status %d", response.StatusCode)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return KeySet{}, errors.New("DDS verification-key response is not JSON")
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, maxPublicKeyResponseBytes+1))
	if err != nil {
		return KeySet{}, fmt.Errorf("read DDS verification keys: %w", err)
	}
	if len(body) > maxPublicKeyResponseBytes {
		return KeySet{}, errors.New("DDS verification-key response is too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var payload verificationKeySetResponse
	if err := decoder.Decode(&payload); err != nil {
		return KeySet{}, fmt.Errorf("decode DDS verification keys: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return KeySet{}, err
	}
	if payload.Version != verificationKeySetVersion || payload.Generation == 0 {
		return KeySet{}, errors.New("DDS verification-key set has an invalid version or generation")
	}
	if payload.PreviousKeyOverlapSeconds == 0 ||
		payload.PreviousKeyOverlapSeconds > uint64(math.MaxInt64/int64(time.Second)) {
		return KeySet{}, errors.New("DDS verification-key set has an invalid previous-key overlap")
	}
	if len(payload.Keys) < 1 || len(payload.Keys) > 2 ||
		payload.Keys[0].Status != verificationKeyCurrent ||
		(len(payload.Keys) == 2 && payload.Keys[1].Status != verificationKeyPrevious) {
		return KeySet{}, errors.New("DDS verification-key set must contain current first and at most one previous key")
	}

	current, err := parseVerificationKey(payload.Keys[0])
	if err != nil {
		return KeySet{}, err
	}
	keySet := KeySet{
		Generation:         payload.Generation,
		PreviousKeyOverlap: time.Duration(payload.PreviousKeyOverlapSeconds) * time.Second,
		Current:            current,
	}
	if len(payload.Keys) == 2 {
		previous, err := parseVerificationKey(payload.Keys[1])
		if err != nil {
			return KeySet{}, err
		}
		if payload.Keys[0].ID == payload.Keys[1].ID {
			return KeySet{}, errors.New("DDS current and previous verification-key IDs must differ")
		}
		keySet.Previous = &previous
	}
	return keySet, nil
}

func parseVerificationKey(key verificationKeyResponse) (Material, error) {
	if len(key.ID) != sha256.Size*2 || strings.ToLower(key.ID) != key.ID {
		return Material{}, errors.New("DDS verification-key ID must be a lowercase SHA-256 fingerprint")
	}
	if _, err := hex.DecodeString(key.ID); err != nil {
		return Material{}, errors.New("DDS verification-key ID must be a lowercase SHA-256 fingerprint")
	}
	method := jwt.GetSigningMethod(key.SigningMethod)
	if method == nil {
		return Material{}, errors.New("DDS verification endpoint returned an unsupported signing method")
	}

	var publicKey crypto.PublicKey
	var err error
	switch method {
	case jwt.SigningMethodRS256, jwt.SigningMethodRS384, jwt.SigningMethodRS512:
		publicKey, err = jwt.ParseRSAPublicKeyFromPEM([]byte(key.PublicKey))
	case jwt.SigningMethodES256, jwt.SigningMethodES384, jwt.SigningMethodES512:
		publicKey, err = jwt.ParseECPublicKeyFromPEM([]byte(key.PublicKey))
	case jwt.SigningMethodEdDSA:
		publicKey, err = jwt.ParseEdPublicKeyFromPEM([]byte(key.PublicKey))
	default:
		err = errors.New("DDS verification endpoint returned an unsupported signing method")
	}
	if err != nil {
		return Material{}, fmt.Errorf("parse DDS verification key: %w", err)
	}
	if fingerprint, err := publishedKeyFingerprint(publicKey); err != nil {
		return Material{}, err
	} else if fingerprint != key.ID {
		return Material{}, errors.New("DDS verification-key ID does not match its public key")
	}
	return Material{PublicKey: publicKey, Method: method}, nil
}

func publishedKeyFingerprint(publicKey crypto.PublicKey) (string, error) {
	encoded, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		return "", fmt.Errorf("encode DDS verification key: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == io.EOF {
		return nil
	} else if err != nil {
		return fmt.Errorf("decode trailing DDS verification-key content: %w", err)
	}
	return errors.New("DDS verification-key response contains trailing JSON")
}
