package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"
)

// cleat#3630. An installed library definition holds no bytes of its own: LoadWASM follows its reference to
// library.members and returns that row's wasm_bytes. library.members carries ck_members_digest
// (wasm_sha256 = sha256(wasm_bytes)), so a plain UPDATE of the bytes is refused (measured, #3630), and the
// bytes can differ from their recorded digest only if a table owner removes the constraint first. That is
// the case this pins: the bytes do NOT match the recorded digest, and the worker must refuse to load them
// instead of running them.
//
// Not covered: a table owner who changes the bytes, the length AND the digest together. Nothing that
// compares bytes with a digest can see that, and this test does not pretend to.
func TestALibraryMemberWhoseBytesDifferFromItsDigestIsNotLoaded(t *testing.T) {
	t.Run("postgres", func(t *testing.T) { runALibraryMemberWhoseBytesDifferFromItsDigest(t, newPostgresLibRig(t)) })
	t.Run("sqlserver", func(t *testing.T) { runALibraryMemberWhoseBytesDifferFromItsDigest(t, newMSSQLLibRig(t)) })
}

func runALibraryMemberWhoseBytesDifferFromItsDigest(t *testing.T, rig *libInstallRig) {
	t.Helper()
	ctx := context.Background()
	const tenant = "f3000000-0000-4000-8000-000000003630"
	rig.registerPlugins(t, "slack-notify")

	library := fmt.Sprintf("lib-3630-%d", time.Now().UnixNano())
	rig.cleanup([]string{tenant}, library)
	t.Cleanup(func() { rig.cleanup([]string{tenant}, library) })

	m := libMember{name: "digest-member", version: 1, exposure: "internal", variant: 3}
	rig.seedMember(t, library, m)
	rig.exec(t, "", `INSERT INTO library.bundles (library, bundle_version, published_by) VALUES (?, ?, 'test')`, library, "1.0.0")
	rig.exec(t, "", `INSERT INTO library.bundle_members (library, bundle_version, name, version) VALUES (?, ?, ?, ?)`,
		library, "1.0.0", m.name, m.version)
	if _, err := rig.store(tenant).InstallLibraryBundle(ctx, InstallLibraryRequest{Library: library, BundleVersion: "1.0.0", InstalledBy: "tester"}); err != nil {
		t.Fatalf("UNMEASURED: the install: %v", err)
	}

	// Control: with the constraint standing, the member loads and its bytes are the ones the digest names.
	orig := m.wasm()
	got, err := rig.store(tenant).LoadWASM(ctx, m.name, m.version)
	if err != nil || string(got) != string(orig) {
		t.Fatalf("UNMEASURED: before any damage LoadWASM = %d bytes, %v; want the member's %d", len(got), err, len(orig))
	}
	stored := rig.query(t, "", `SELECT wasm_sha256 FROM library.members WHERE library = ? AND name = ? AND version = ?`, library, m.name, m.version)
	if len(stored) != 1 {
		t.Fatalf("UNMEASURED: member row missing")
	}
	sum := sha256.Sum256(orig)
	if stored[0]["wasm_sha256"] != hex.EncodeToString(sum[:]) {
		t.Fatalf("UNMEASURED: the recorded digest %s is not sha256 of the bytes %s", stored[0]["wasm_sha256"], hex.EncodeToString(sum[:]))
	}

	// The damage, as a table owner: remove the constraint, then change the bytes and keep the length. The
	// restore is registered FIRST so a failure below cannot leave the schema without its constraint.
	altered := append([]byte(nil), orig...)
	altered[0] ^= 0xff
	restore := func() {
		rig.exec(t, "", `UPDATE library.members SET wasm_bytes = ? WHERE library = ? AND name = ? AND version = ?`, orig, library, m.name, m.version)
		if rig.name == "postgres" {
			rig.exec(t, "", `DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ck_members_digest' AND conrelid = 'library.members'::regclass) THEN
				ALTER TABLE library.members ADD CONSTRAINT ck_members_digest CHECK (wasm_sha256 = encode(sha256(wasm_bytes), 'hex')); END IF; END $$`)
		} else {
			rig.exec(t, "", `ALTER TABLE library.members WITH CHECK CHECK CONSTRAINT ck_members_digest`)
		}
	}
	t.Cleanup(restore)
	if rig.name == "postgres" {
		rig.exec(t, "", `ALTER TABLE library.members DROP CONSTRAINT ck_members_digest`)
	} else {
		rig.exec(t, "", `ALTER TABLE library.members NOCHECK CONSTRAINT ck_members_digest`)
	}
	rig.exec(t, "", `UPDATE library.members SET wasm_bytes = ? WHERE library = ? AND name = ? AND version = ?`, altered, library, m.name, m.version)

	// Precondition: the damage is real. The recorded digest is untouched and no longer names the bytes.
	after := rig.query(t, "", `SELECT wasm_sha256 FROM library.members WHERE library = ? AND name = ? AND version = ?`, library, m.name, m.version)
	asum := sha256.Sum256(altered)
	if len(after) != 1 || after[0]["wasm_sha256"] != stored[0]["wasm_sha256"] || after[0]["wasm_sha256"] == hex.EncodeToString(asum[:]) {
		t.Fatalf("UNMEASURED: the digest should be untouched and no longer name the altered bytes: %v", after)
	}

	loaded, err := rig.store(tenant).LoadWASM(ctx, m.name, m.version)
	if err == nil {
		t.Fatalf("%s: LoadWASM loaded %d bytes whose sha256 is %s, but the member's recorded digest is %s", rig.name, len(loaded), hex.EncodeToString(asum[:]), after[0]["wasm_sha256"])
	}
	if len(loaded) != 0 {
		t.Errorf("%s: LoadWASM returned %d bytes alongside its refusal; it must return none", rig.name, len(loaded))
	}
	if !errors.Is(err, ErrLibraryMemberDigestMismatch) {
		t.Fatalf("%s: LoadWASM refused with %v, want ErrLibraryMemberDigestMismatch: a refusal for any other reason (a dead connection, a missing grant) is not this check", rig.name, err)
	}
	var mm *LibraryMemberDigestMismatchError
	if !errors.As(err, &mm) {
		t.Fatalf("%s: the refusal %v does not carry a *LibraryMemberDigestMismatchError", rig.name, err)
	}
	if mm.Library != library || mm.Name != m.name || mm.Version != m.version ||
		mm.Recorded != after[0]["wasm_sha256"] || mm.Computed != hex.EncodeToString(asum[:]) {
		t.Errorf("%s: the refusal names %+v; want library %q, member %s v%d, recorded %s, computed %s",
			rig.name, *mm, library, m.name, m.version, after[0]["wasm_sha256"], hex.EncodeToString(asum[:]))
	}

	// The refusal is about THESE bytes: put them back and the same definition loads again, so what was
	// refused was the mismatch and not the definition.
	restore()
	again, err := rig.store(tenant).LoadWASM(ctx, m.name, m.version)
	if err != nil || string(again) != string(orig) {
		t.Errorf("%s: after the bytes were put back LoadWASM = %d bytes, %v; want the member's %d", rig.name, len(again), err, len(orig))
	}
}
