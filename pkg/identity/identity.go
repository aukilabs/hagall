package identity

import (
	"crypto/ecdsa"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/aukilabs/hagall/pkg/models"
	ethcommon "github.com/ethereum/go-ethereum/common"
	ethcrypto "github.com/ethereum/go-ethereum/crypto"
	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
	cryptopb "github.com/libp2p/go-libp2p/core/crypto/pb"
	"github.com/libp2p/go-libp2p/core/peer"
)

const maxSecretFileBytes = 64 << 10

// Files names the three independent, operator-provisioned secret files owned
// by a relay process. Secret values are deliberately not accepted directly.
type Files struct {
	RegistrationCredentials string
	WalletPrivateKey        string
	Libp2pPrivateKey        string
}

// Loaded is the stable identity material used by DDS registration/auth and the
// libp2p host. RegistrationCredentials remains opaque; only NodeID is exposed
// from its validated envelope.
type Loaded struct {
	RegistrationCredentials string
	NodeID                  string
	WalletPrivateKey        *ecdsa.PrivateKey
	WalletAddress           ethcommon.Address
	Libp2pPrivateKey        libp2pcrypto.PrivKey
	PeerID                  peer.ID
}

func Load(files Files) (*Loaded, error) {
	registrationCredentials, nodeID, err := loadRegistrationCredentials(files.RegistrationCredentials)
	if err != nil {
		return nil, err
	}
	walletPrivateKey, err := loadWalletPrivateKey(files.WalletPrivateKey)
	if err != nil {
		return nil, err
	}
	libp2pPrivateKey, peerID, err := loadLibp2pPrivateKey(files.Libp2pPrivateKey)
	if err != nil {
		return nil, err
	}
	return &Loaded{
		RegistrationCredentials: registrationCredentials,
		NodeID:                  nodeID,
		WalletPrivateKey:        walletPrivateKey,
		WalletAddress:           ethcrypto.PubkeyToAddress(walletPrivateKey.PublicKey),
		Libp2pPrivateKey:        libp2pPrivateKey,
		PeerID:                  peerID,
	}, nil
}

func loadRegistrationCredentials(path string) (string, string, error) {
	contents, err := readSecretFile(path, "registration credentials")
	if err != nil {
		return "", "", err
	}
	encoded := strings.TrimSpace(string(contents))
	if encoded == "" {
		return "", "", fmt.Errorf("registration credentials secret is empty")
	}
	credentials, err := models.ValidateRegistrationCredentials(encoded)
	if err != nil {
		return "", "", fmt.Errorf("invalid registration credentials secret: %w", err)
	}
	return encoded, credentials.ID, nil
}

func loadWalletPrivateKey(path string) (*ecdsa.PrivateKey, error) {
	contents, err := readSecretFile(path, "wallet private key")
	if err != nil {
		return nil, err
	}
	hexKey := strings.TrimSpace(string(contents))
	hexKey = strings.TrimPrefix(hexKey, "0x")
	if hexKey == "" {
		return nil, fmt.Errorf("wallet private key secret is empty")
	}
	privateKey, err := ethcrypto.HexToECDSA(hexKey)
	if err != nil {
		return nil, fmt.Errorf("invalid wallet private key secret: %w", err)
	}
	return privateKey, nil
}

func loadLibp2pPrivateKey(path string) (libp2pcrypto.PrivKey, peer.ID, error) {
	contents, err := readSecretFile(path, "libp2p private key")
	if err != nil {
		return nil, "", err
	}
	privateKey, err := libp2pcrypto.UnmarshalPrivateKey(contents)
	if err != nil {
		return nil, "", fmt.Errorf("invalid libp2p private key secret: %w", err)
	}
	if privateKey.Type() != cryptopb.KeyType_Ed25519 {
		return nil, "", fmt.Errorf("libp2p private key secret must contain an Ed25519 key")
	}
	peerID, err := peer.IDFromPrivateKey(privateKey)
	if err != nil {
		return nil, "", fmt.Errorf("derive libp2p peer ID: %w", err)
	}
	return privateKey, peerID, nil
}

func readSecretFile(path, label string) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("%s file path is required", label)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, secretFileError("open", label, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, secretFileError("stat", label, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s file must be a regular file", label)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s file must not grant group or other permissions", label)
	}
	if info.Size() == 0 {
		return nil, fmt.Errorf("%s file is empty", label)
	}
	if info.Size() > maxSecretFileBytes {
		return nil, fmt.Errorf("%s file exceeds %d bytes", label, maxSecretFileBytes)
	}
	contents, err := io.ReadAll(io.LimitReader(file, maxSecretFileBytes+1))
	if err != nil {
		return nil, secretFileError("read", label, err)
	}
	if len(contents) > maxSecretFileBytes {
		return nil, fmt.Errorf("%s file exceeds %d bytes", label, maxSecretFileBytes)
	}
	if len(contents) == 0 {
		return nil, fmt.Errorf("%s file is empty", label)
	}
	return contents, nil
}

// secretFileError preserves only a coarse error class. Operating-system path
// errors embed the configured secret mount path, which must never escape
// through startup logs or status surfaces.
func secretFileError(operation, label string, err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%s %s file: %w", operation, label, fs.ErrNotExist)
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("%s %s file: %w", operation, label, fs.ErrPermission)
	default:
		return fmt.Errorf("%s %s file: secret file I/O failed", operation, label)
	}
}
