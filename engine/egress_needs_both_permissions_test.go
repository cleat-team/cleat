package engine

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
)

// cleat#1565, owner decision 2026-09-15: a destination is reachable only when
// the OPERATOR permits it AND the TENANT permits it.
//
// Before this the operator had no say at all in what their own deployment
// could reach: a tenant's list was the only thing consulted, and the private
// range denial -- which reads like operator policy -- was a hardcoded table
// referenced nowhere else. A default nothing consults is indistinguishable
// from a policy nobody set.
func TestEgressNeedsBothOperatorAndTenantPermission(t *testing.T) {
	dialed := func(g *EgressGuard) (string, error) {
		var got string
		g.Dial = func(_ context.Context, _, address string) (net.Conn, error) {
			got = address
			return nil, errors.New("stop: the decision is what is under test")
		}
		g.lookup = func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		}
		_, err := g.DialContext(context.Background(), "tcp", "api.example.com:443")
		return got, err
	}

	for _, tc := range []struct {
		name          string
		operator      *HostAllowlist
		tenant        *HostAllowlist
		wantReach     bool
		wantRefusedBy string
	}{
		{
			name:      "both permit",
			operator:  NewHostAllowlist("api.example.com"),
			tenant:    NewHostAllowlist("api.example.com"),
			wantReach: true,
		},
		{
			name:          "operator permits, tenant does not",
			operator:      NewHostAllowlist("api.example.com"),
			tenant:        NewHostAllowlist("elsewhere.example"),
			wantRefusedBy: "tenant",
		},
		{
			name:          "tenant permits, operator does not",
			operator:      NewHostAllowlist("only-this.example"),
			tenant:        NewHostAllowlist("api.example.com"),
			wantRefusedBy: "operator",
		},
		{
			name:          "neither",
			operator:      NewHostAllowlist("a.example"),
			tenant:        NewHostAllowlist("b.example"),
			wantRefusedBy: "operator", // the outer bound answers first
		},
		{
			// The owner's decision: an unset operator list permits every
			// public host, so a deployment that configures nothing still
			// works and the burden sits with the tenant.
			name:      "operator UNSET permits any public host",
			operator:  NewHostAllowlist(),
			tenant:    NewHostAllowlist("api.example.com"),
			wantReach: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var opFn func(context.Context, string) (bool, error)
			if len(tc.operator.Entries()) > 0 {
				opFn = tc.operator.AllowHostFunc()
			}
			g := &EgressGuard{OperatorAllows: opFn, AllowHost: tc.tenant.AllowHostFunc()}
			addr, err := dialed(g)

			if tc.wantReach {
				if addr != "93.184.216.34:443" {
					t.Fatalf("did not reach the host: dialled %q, err %v", addr, err)
				}
				return
			}
			var denied *EgressDeniedError
			if !errors.As(err, &denied) {
				t.Fatalf("got %v, want an EgressDeniedError", err)
			}
			if addr != "" {
				t.Errorf("refused, but still dialled %q", addr)
			}
			// The refusal must name WHICH layer said no -- otherwise an
			// operator edits a tenant's list, or the reverse.
			if !strings.Contains(denied.Reason, tc.wantRefusedBy) {
				t.Errorf("refusal blames the wrong layer: %q does not mention %q",
					denied.Reason, tc.wantRefusedBy)
			}
		})
	}
}

// Egress with NO tenant -- plugin sweeps, and auth-exempt routes such as the
// OAuth callback, which records in its own code that "the state parameter
// identifies the tenant, so there is none to scope by".
//
// The operator layer governs those alone. TenantOptional is explicit rather
// than inferred from a nil tenant hook, because "there is no tenant" and "the
// tenant lookup produced nothing" must not be the same state: the second is a
// bug that has to deny, and conflating them turns it into an open gate.
func TestTenantlessEgressAnswersToTheOperatorAlone(t *testing.T) {
	run := func(g *EgressGuard) error {
		g.Dial = func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("reached the dial")
		}
		g.lookup = func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		}
		_, err := g.DialContext(context.Background(), "tcp", "kafka-proxy.example:8082")
		return err
	}

	t.Run("operator permits it, so a sweep reaches it with no tenant", func(t *testing.T) {
		err := run(&EgressGuard{
			TenantOptional: func(context.Context) bool { return true },
			OperatorAllows: NewHostAllowlist("kafka-proxy.example").AllowHostFunc(),
		})
		if err == nil || !strings.Contains(err.Error(), "reached the dial") {
			t.Fatalf("a permitted sweep did not reach the dial: %v", err)
		}
	})

	t.Run("operator does not, so it is refused", func(t *testing.T) {
		err := run(&EgressGuard{
			TenantOptional: func(context.Context) bool { return true },
			OperatorAllows: NewHostAllowlist("something.else").AllowHostFunc(),
		})
		var denied *EgressDeniedError
		if !errors.As(err, &denied) {
			t.Fatalf("got %v, want an EgressDeniedError", err)
		}
		if !strings.Contains(denied.Reason, "operator") {
			t.Errorf("refusal does not name the operator layer: %q", denied.Reason)
		}
	})

	t.Run("the floor still applies with no tenant", func(t *testing.T) {
		// Found by a mis-aimed falsification: a mutation that let a
		// tenant-less call skip the floor left every test green. The
		// tenant-less path is the one that answers to the fewest layers, so it
		// is the one where a missing floor check would be least noticed.
		g := &EgressGuard{
			TenantOptional: func(context.Context) bool { return true },
			OperatorAllows: NewHostAllowlist("metadata.example").AllowHostFunc(),
			Dial: func(context.Context, string, string) (net.Conn, error) {
				t.Fatal("a tenant-less call reached a floor-denied address")
				return nil, nil
			},
		}
		g.lookup = func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
		}
		_, err := g.DialContext(context.Background(), "tcp", "metadata.example:80")
		var denied *EgressDeniedError
		if !errors.As(err, &denied) {
			t.Fatalf("got %v, want an EgressDeniedError", err)
		}
		if !strings.Contains(denied.Reason, "metadata") {
			t.Errorf("refused, but not by the floor: %q", denied.Reason)
		}
	})

	t.Run("a MISSING tenant hook without TenantOptional still denies", func(t *testing.T) {
		// The bug case: a call that should have had a tenant and did not.
		err := run(&EgressGuard{OperatorAllows: NewHostAllowlist("kafka-proxy.example").AllowHostFunc()})
		var denied *EgressDeniedError
		if !errors.As(err, &denied) {
			t.Fatalf("got %v, want a refusal -- an absent tenant policy is not permission", err)
		}
		if !strings.Contains(denied.Reason, "tenant") {
			t.Errorf("refusal does not name the tenant layer: %q", denied.Reason)
		}
	})
}

// Neither layer can admit a private address: a host on either list that
// resolves into private space is the rebinding case, not a grant.
func TestNeitherLayerCanAdmitAPrivateAddress(t *testing.T) {
	g := &EgressGuard{
		OperatorAllows: NewHostAllowlist("metadata.example").AllowHostFunc(),
		AllowHost:      NewHostAllowlist("metadata.example").AllowHostFunc(),
		Dial: func(context.Context, string, string) (net.Conn, error) {
			t.Fatal("dialled a floor-denied address that both layers permitted")
			return nil, nil
		},
	}
	g.lookup = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
	}
	_, err := g.DialContext(context.Background(), "tcp", "metadata.example:80")
	var denied *EgressDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("got %v, want an EgressDeniedError", err)
	}
	if !strings.Contains(denied.Reason, "metadata") {
		t.Errorf("refused, but not by the floor: %q", denied.Reason)
	}
}
