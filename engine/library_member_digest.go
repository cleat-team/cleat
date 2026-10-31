package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

// ErrLibraryMemberDigestMismatch is what loading an installed library definition returns when the member's
// bytes do not hash to the digest recorded beside them (cleat#3630). library.members refuses that state
// through ck_members_digest, so it exists only after a table owner removed the constraint; the load refuses
// it instead of running the bytes.
var ErrLibraryMemberDigestMismatch = errors.New("library member bytes do not match their recorded digest")

// LibraryMemberDigestMismatchError carries which member, and both digests, so the refusal names what to look at.
type LibraryMemberDigestMismatchError struct {
	Library  string
	Name     string
	Version  int
	Recorded string
	Computed string
}

func (e *LibraryMemberDigestMismatchError) Error() string {
	return fmt.Sprintf("refusing to load workflow %q version %d: the bytes of its library member %s/%s v%d hash to sha256 %s but the member records %s. "+
		"The library schema's ck_members_digest constraint refuses that state, so it was removed or bypassed; the member is not run",
		e.Name, e.Version, e.Library, e.Name, e.Version, e.Computed, e.Recorded)
}

// Is reports a match against ErrLibraryMemberDigestMismatch.
func (e *LibraryMemberDigestMismatchError) Is(target error) bool {
	return target == ErrLibraryMemberDigestMismatch
}

// verifyLibraryMemberDigest compares sha256(wasm) with the digest the member records. The recorded digest is
// compared as lower-case hex: ck_members_digest holds it that way on both dialects.
func verifyLibraryMemberDigest(library, name string, version int, recorded string, wasm []byte) error {
	sum := sha256.Sum256(wasm)
	computed := hex.EncodeToString(sum[:])
	if computed != recorded {
		return &LibraryMemberDigestMismatchError{Library: library, Name: name, Version: version, Recorded: recorded, Computed: computed}
	}
	return nil
}
