package identity

import (
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"

	"github.com/libp2p/go-libp2p/core/crypto"
)

func LoadOrCreate(path string) (crypto.PrivKey, error) {
	if data, err := os.ReadFile(path); err == nil {
		raw, err := base64.StdEncoding.DecodeString(string(data))
		if err != nil {
			return nil, err
		}
		return crypto.UnmarshalPrivateKey(raw)
	}

	key, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		return nil, err
	}
	raw, err := crypto.MarshalPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	encoded := base64.StdEncoding.EncodeToString(raw)
	if err := os.WriteFile(path, []byte(encoded), 0o600); err != nil {
		return nil, err
	}
	return key, nil
}
