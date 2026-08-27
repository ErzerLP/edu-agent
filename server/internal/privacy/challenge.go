package privacy

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
)

const offlinePurgeChallengeDomain = "edu-agent-offline-purge-v1"

type OfflineChallengeKeyring struct {
	current int
	keys    map[int][]byte
}

func NewOfflineChallengeKeyring(keys map[int][]byte) (*OfflineChallengeKeyring, error) {
	if len(keys) == 0 {
		return nil, errors.New("offline purge challenge keyring is empty")
	}
	versions := make([]int, 0, len(keys))
	copied := make(map[int][]byte, len(keys))
	for version, key := range keys {
		if version <= 0 || len(key) != sha256.Size {
			return nil, errors.New("offline purge challenge keys require positive versions and 32-byte keys")
		}
		copied[version] = append([]byte(nil), key...)
		versions = append(versions, version)
	}
	sort.Ints(versions)
	return &OfflineChallengeKeyring{current: versions[len(versions)-1], keys: copied}, nil
}

func (k *OfflineChallengeKeyring) CurrentVersion() int {
	if k == nil {
		return 0
	}
	return k.current
}

func (k *OfflineChallengeKeyring) Challenge(version int, erasureID, deviceID string, sourceGeneration, targetGeneration, revision int64) (string, error) {
	if k == nil {
		return "", &Error{Code: CodeOfflineChallengeUnavailable, Reason: "offline_purge_challenge_keyring_unavailable"}
	}
	key, ok := k.keys[version]
	if !ok {
		return "", &Error{Code: CodeOfflineChallengeUnavailable, Reason: "offline_purge_challenge_key_version_unavailable"}
	}
	message := fmt.Sprintf("%s\n%s\n%s\n%d\n%d\n%d", offlinePurgeChallengeDomain, erasureID, deviceID, sourceGeneration, targetGeneration, revision)
	mac := hmac.New(sha256.New, key)
	if _, err := mac.Write([]byte(message)); err != nil {
		return "", fmt.Errorf("compute offline purge challenge: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (k *OfflineChallengeKeyring) Verify(version int, erasureID, deviceID string, sourceGeneration, targetGeneration, revision int64, challenge string) bool {
	expected, err := k.Challenge(version, erasureID, deviceID, sourceGeneration, targetGeneration, revision)
	if err != nil || len(expected) != len(challenge) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(challenge)) == 1
}

func OfflineChallengeDigest(challenge string) [sha256.Size]byte {
	return sha256.Sum256([]byte(challenge))
}
