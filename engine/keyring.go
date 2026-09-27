package engine

import (
	"crypto/subtle"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// VersionedKey is one 32-byte key and the version rows sealed under it carry.
//
// NOTHING HERE IS SPECIFIC TO TENANT SECRETS, and that is deliberate: cleat#1992
// gives payload encryption the same rotation, and it should reuse this rather
// than copy it. What IS specific to secrets -- the environment variables, the
// key_version column, the census and reseal -- stays in tenant_secrets.go and
// tenant_secrets_rotation.go.
type VersionedKey struct {
	Version int
	Key     []byte
}

// KeyRing is the current key, which seals every new write, and any number of
// previous keys, which are only ever used to READ what was sealed before a
// rotation.
//
// THE VERSION IS AN INTEGER THE OPERATOR DECLARES, and not a fingerprint of the
// key, because the column it must match is an INTEGER on all three dialects and
// because a declared number is what an operator can read in a log line and in a
// query result. The current key defaults to version 1 where a caller loads it
// from configuration, which is what every pre-rotation row already carries.
//
// It is immutable once built. A holder that is handed a different ring is a
// different holder, built afresh; nothing here reloads keys. (Reloading is
// cleat#1992's, and it should swap a whole ring, not edit one.)
type KeyRing struct {
	current  VersionedKey
	previous []VersionedKey
}

// NewKeyRing validates and builds a ring. Every key must be 32 bytes,
// every version must be a positive integer, no two keys may share a version,
// and no two versions may carry the same key bytes.
//
// The last rule is not tidiness. Two versions holding one key would open every
// row under either, so a rotation configured that way would report success
// while changing nothing about which key protects the data.
func NewKeyRing(current VersionedKey, previous ...VersionedKey) (*KeyRing, error) {
	all := append([]VersionedKey{current}, previous...)
	for i, k := range all {
		if k.Version < 1 {
			return nil, fmt.Errorf("key version must be a positive integer, got %d", k.Version)
		}
		if len(k.Key) != 32 {
			return nil, fmt.Errorf("key (version %d) must be 32 bytes, got %d", k.Version, len(k.Key))
		}
		for _, other := range all[:i] {
			if other.Version == k.Version {
				return nil, fmt.Errorf("key version %d is configured twice", k.Version)
			}
			if subtle.ConstantTimeCompare(other.Key, k.Key) == 1 {
				return nil, fmt.Errorf("key versions %d and %d carry the same key: a rotation "+
					"to a key that is not new protects nothing", other.Version, k.Version)
			}
		}
	}
	return &KeyRing{current: current, previous: append([]VersionedKey(nil), previous...)}, nil
}

// Current is the key that seals every new write.
func (r *KeyRing) Current() VersionedKey { return r.current }

// Key returns the key that carries this version, and whether the ring holds one.
func (r *KeyRing) Key(version int) (VersionedKey, bool) {
	if r == nil {
		return VersionedKey{}, false
	}
	if r.current.Version == version {
		return r.current, true
	}
	for _, k := range r.previous {
		if k.Version == version {
			return k, true
		}
	}
	return VersionedKey{}, false
}

// Versions lists every version this ring can open, ascending.
func (r *KeyRing) Versions() []int {
	if r == nil {
		return nil
	}
	vs := []int{r.current.Version}
	for _, k := range r.previous {
		vs = append(vs, k.Version)
	}
	sort.Ints(vs)
	return vs
}

// KeyVersionFromEnv parses a key version from configuration, for any ring loaded from
// the environment (cleat#1992 should call this, not re-implement it). An empty value gives def; 0 as def means
// "not given", which the caller turns into its own error.
func KeyVersionFromEnv(v string, def int, name string) (int, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s must be a positive integer, got %q", name, v)
	}
	return n, nil
}
