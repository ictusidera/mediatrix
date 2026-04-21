package p2p

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"
)

func serviceKey(name string) string {
	if strings.HasPrefix(name, "service:") {
		return name
	}
	return "service:" + name
}

func fileKeyFromDigest(digest string) string {
	return "file:sha256:" + strings.ToLower(digest)
}

func cidForKey(namespace, key string) (cid.Cid, error) {
	sum := sha256.Sum256([]byte(namespace + ":" + key))
	mh, err := multihash.Encode(sum[:], multihash.SHA2_256)
	if err != nil {
		return cid.Undef, err
	}
	return cid.NewCidV1(cid.Raw, mh), nil
}

func parseFileKey(key string) (string, error) {
	const prefix = "file:sha256:"
	if !strings.HasPrefix(key, prefix) {
		return "", fmt.Errorf("file key must start with %q", prefix)
	}
	digest := strings.TrimPrefix(key, prefix)
	if len(digest) != sha256.Size*2 {
		return "", fmt.Errorf("sha256 digest must be %d hex characters", sha256.Size*2)
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return "", err
	}
	return strings.ToLower(digest), nil
}
