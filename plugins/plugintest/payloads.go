package plugintest

import (
	"bytes"
	"context"
	"fmt"

	"github.com/cleat-team/cleat/plugin"
)

// FakePayloads is a reversible, tenant-bound stand-in for plugin.Payloads,
// playing the same role for a plugin's own tests that FakeSecrets plays for
// plugin.Secrets. cleat#1992. oauthprovider's session/access/refresh tokens
// are plugin.Payloads' first production caller; this is its first fake.
//
// NOT REAL CRYPTO, deliberately. Nothing here tests engine.PayloadEncryption
// itself -- engine/encryption_test.go does that. What a plugin's own test
// needs to verify is that IT calls Seal before storing and Open (if it ever
// reads back) after loading, and that a value sealed under one tenant cannot
// be opened under another. A fixed, reversible transform proves both without
// pulling AEAD into every plugin's test binary.
//
// The transform: sealed = tenantID + 0x00 + plaintext. Open splits on the
// first 0x00 and refuses unless the embedded tenant matches ctx's --
// mirroring the real adapter's AAD binding (SealForPlugin/OpenForPlugin
// derive under the tenant, so a value sealed for tenant A does not open
// under tenant B's key) without needing a key at all.
type FakePayloads struct{}

// NewFakePayloads returns a FakePayloads. It carries no state -- tenant
// binding happens per call, not per instance -- so the zero value is already
// usable; the constructor exists for symmetry with NewFakeSecrets.
func NewFakePayloads() *FakePayloads {
	return &FakePayloads{}
}

func (FakePayloads) Seal(ctx context.Context, plaintext []byte) ([]byte, error) {
	tid, err := tenantIDFromContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("plugintest.FakePayloads: %w", err)
	}
	sealed := make([]byte, 0, len(tid)+1+len(plaintext))
	sealed = append(sealed, tid...)
	sealed = append(sealed, 0)
	sealed = append(sealed, plaintext...)
	return sealed, nil
}

func (FakePayloads) Open(ctx context.Context, sealed []byte) ([]byte, error) {
	tid, err := tenantIDFromContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("plugintest.FakePayloads: %w", err)
	}
	i := bytes.IndexByte(sealed, 0)
	if i < 0 {
		return nil, fmt.Errorf("plugintest.FakePayloads: Open: not a value Seal produced (no tenant separator)")
	}
	sealedTenant, plaintext := string(sealed[:i]), sealed[i+1:]
	if sealedTenant != tid {
		return nil, fmt.Errorf("plugintest.FakePayloads: Open: sealed for tenant %s, opened as tenant %s", sealedTenant, tid)
	}
	return plaintext, nil
}

var _ plugin.Payloads = (*FakePayloads)(nil)
