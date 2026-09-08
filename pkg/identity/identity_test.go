package identity

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ethcrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/google/uuid"
	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/stretchr/testify/require"
)

func writeSecret(t *testing.T, directory, name string, contents []byte, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(directory, name)
	require.NoError(t, os.WriteFile(path, contents, mode))
	require.NoError(t, os.Chmod(path, mode))
	return path
}

func validIdentityFiles(t *testing.T) (Files, string) {
	t.Helper()
	directory := t.TempDir()
	nodeID := uuid.NewString()
	credentials := base64.StdEncoding.EncodeToString([]byte(nodeID + ":registration-secret"))
	walletKey, err := ethcrypto.GenerateKey()
	require.NoError(t, err)
	libp2pKey, _, err := libp2pcrypto.GenerateEd25519Key(rand.Reader)
	require.NoError(t, err)
	encodedLibp2pKey, err := libp2pcrypto.MarshalPrivateKey(libp2pKey)
	require.NoError(t, err)
	return Files{
		RegistrationCredentials: writeSecret(t, directory, "credentials", []byte(credentials+"\n"), 0o600),
		WalletPrivateKey: writeSecret(
			t,
			directory,
			"wallet",
			[]byte("  0x"+hex.EncodeToString(ethcrypto.FromECDSA(walletKey))+"\n"),
			0o600,
		),
		Libp2pPrivateKey: writeSecret(t, directory, "libp2p", encodedLibp2pKey, 0o600),
	}, nodeID
}

func TestLoadReloadsStableIndependentIdentities(t *testing.T) {
	files, nodeID := validIdentityFiles(t)
	before, err := directoryEntries(filepath.Dir(files.Libp2pPrivateKey))
	require.NoError(t, err)

	first, err := Load(files)
	require.NoError(t, err)
	second, err := Load(files)
	require.NoError(t, err)
	require.Equal(t, nodeID, first.NodeID)
	require.Equal(t, first.NodeID, second.NodeID)
	require.Equal(t, first.RegistrationCredentials, second.RegistrationCredentials)
	require.Equal(t, first.WalletAddress, second.WalletAddress)
	require.Equal(t, first.PeerID, second.PeerID)
	require.True(t, first.Libp2pPrivateKey.Equals(second.Libp2pPrivateKey))
	require.Equal(t, ethcrypto.FromECDSA(first.WalletPrivateKey), ethcrypto.FromECDSA(second.WalletPrivateKey))
	require.NotEqual(t, first.WalletAddress.Hex(), first.PeerID.String())

	after, err := directoryEntries(filepath.Dir(files.Libp2pPrivateKey))
	require.NoError(t, err)
	require.Equal(t, before, after, "loading identity must not create files")
}

func TestLoadRejectsMissingEmptyNonRegularAndPermissiveSecrets(t *testing.T) {
	files, _ := validIdentityFiles(t)

	t.Run("missing", func(t *testing.T) {
		candidate := files
		candidate.Libp2pPrivateKey = filepath.Join(t.TempDir(), "missing-secret-name")
		_, err := Load(candidate)
		require.ErrorContains(t, err, "open libp2p private key")
		require.False(t, strings.Contains(err.Error(), candidate.Libp2pPrivateKey))
		require.NotContains(t, err.Error(), "missing-secret-name")
	})

	t.Run("empty", func(t *testing.T) {
		candidate := files
		candidate.RegistrationCredentials = writeSecret(t, t.TempDir(), "empty", nil, 0o600)
		_, err := Load(candidate)
		require.ErrorContains(t, err, "empty")
	})

	t.Run("directory", func(t *testing.T) {
		candidate := files
		candidate.WalletPrivateKey = t.TempDir()
		_, err := Load(candidate)
		require.ErrorContains(t, err, "regular file")
	})

	t.Run("permissions", func(t *testing.T) {
		candidate := files
		candidate.RegistrationCredentials = writeSecret(t, t.TempDir(), "credentials", []byte("secret"), 0o640)
		_, err := Load(candidate)
		require.ErrorContains(t, err, "group or other")
	})

	t.Run("oversized", func(t *testing.T) {
		candidate := files
		candidate.RegistrationCredentials = writeSecret(t, t.TempDir(), "credentials", make([]byte, maxSecretFileBytes+1), 0o600)
		_, err := Load(candidate)
		require.ErrorContains(t, err, "exceeds")
	})
}

func TestLoadRejectsInvalidSecretPayloads(t *testing.T) {
	files, _ := validIdentityFiles(t)

	t.Run("credentials", func(t *testing.T) {
		candidate := files
		candidate.RegistrationCredentials = writeSecret(t, t.TempDir(), "credentials", []byte("not-base64"), 0o600)
		_, err := Load(candidate)
		require.ErrorContains(t, err, "invalid registration credentials")
	})

	t.Run("wallet", func(t *testing.T) {
		candidate := files
		candidate.WalletPrivateKey = writeSecret(t, t.TempDir(), "wallet", []byte("not-hex"), 0o600)
		_, err := Load(candidate)
		require.ErrorContains(t, err, "invalid wallet")
	})

	t.Run("libp2p encoding", func(t *testing.T) {
		candidate := files
		candidate.Libp2pPrivateKey = writeSecret(t, t.TempDir(), "libp2p", []byte("not-protobuf"), 0o600)
		_, err := Load(candidate)
		require.ErrorContains(t, err, "invalid libp2p")
	})

	t.Run("libp2p key type", func(t *testing.T) {
		candidate := files
		key, _, err := libp2pcrypto.GenerateKeyPair(libp2pcrypto.Secp256k1, -1)
		require.NoError(t, err)
		encoded, err := libp2pcrypto.MarshalPrivateKey(key)
		require.NoError(t, err)
		candidate.Libp2pPrivateKey = writeSecret(t, t.TempDir(), "libp2p", encoded, 0o600)
		_, err = Load(candidate)
		require.ErrorContains(t, err, "Ed25519")
	})
}

func directoryEntries(path string) ([]string, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(entries))
	for _, entry := range entries {
		result = append(result, entry.Name())
	}
	return result, nil
}
