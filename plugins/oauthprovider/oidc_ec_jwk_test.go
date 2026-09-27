package oauthprovider

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
)

// parseJWK decides whether an identity provider's published EC key is a key at all, so the tests below are the
// contract for what it accepts and refuses. They were written against the hand-built `ecdsa.PublicKey{X, Y}` plus
// `IsOnCurve` implementation and pass unchanged against `ecdsa.ParseUncompressedPublicKey` (cleat#2300).

var ecCurves = []struct {
	crv   string
	curve elliptic.Curve
	size  int // coordinate size in bytes
}{
	{"P-256", elliptic.P256(), 32},
	{"P-384", elliptic.P384(), 48},
	{"P-521", elliptic.P521(), 66},
}

func ecJWK(crv string, x, y []byte) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"kty": "EC", "kid": "ec1", "use": "sig", "crv": crv,
		"x": base64.RawURLEncoding.EncodeToString(x),
		"y": base64.RawURLEncoding.EncodeToString(y),
	})
	return raw
}

func padded(n *big.Int, size int) []byte { return n.FillBytes(make([]byte, size)) }

func TestAValidECJWKIsAcceptedOnEveryNISTCurve(t *testing.T) {
	for _, c := range ecCurves {
		t.Run(c.crv, func(t *testing.T) {
			key, err := ecdsa.GenerateKey(c.curve, rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			got, kid, err := parseJWK(ecJWK(c.crv, padded(key.X, c.size), padded(key.Y, c.size)))
			if err != nil {
				t.Fatalf("a valid %s key was refused: %v", c.crv, err)
			}
			if kid != "ec1" {
				t.Errorf("kid = %q", kid)
			}
			pub, ok := got.(*ecdsa.PublicKey)
			if !ok || !pub.Equal(&key.PublicKey) {
				t.Fatalf("parsed key is not the key that was published: %#v", got)
			}
		})
	}
}

// RFC 7518 wants full-size coordinates, and some providers strip the leading zero byte anyway. Refusing those
// would take a working sign-in away from a real deployment, so the leniency is deliberate and pinned.
func TestAnECJWKWithAStrippedLeadingZeroIsStillAccepted(t *testing.T) {
	c := ecCurves[0]
	for i := 0; i < 20000; i++ {
		key, err := ecdsa.GenerateKey(c.curve, rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		if len(key.X.Bytes()) == c.size && len(key.Y.Bytes()) == c.size {
			continue
		}
		got, _, err := parseJWK(ecJWK(c.crv, key.X.Bytes(), key.Y.Bytes()))
		if err != nil {
			t.Fatalf("a valid key with a stripped coordinate was refused: %v", err)
		}
		if !got.(*ecdsa.PublicKey).Equal(&key.PublicKey) {
			t.Fatal("stripped-coordinate key parsed to a different key")
		}
		return
	}
	// About 1 key in 128 has a leading zero byte in x or y, so 20000 tries all missing is a probability of e^-150.
	t.Fatal("did not generate a P-256 key with a leading zero byte in 20000 tries")
}

// A coordinate is a number, so leading zero bytes are padding whatever their count. The hand-built implementation
// read it with SetBytes and never noticed; this pins that the replacement does not start refusing them.
func TestAnECJWKCoordinateWithExtraLeadingZeroBytesIsStillAccepted(t *testing.T) {
	c := ecCurves[0]
	key, _ := ecdsa.GenerateKey(c.curve, rand.Reader)
	x := append([]byte{0, 0}, padded(key.X, c.size)...)
	y := append([]byte{0}, padded(key.Y, c.size)...)
	got, _, err := parseJWK(ecJWK("P-256", x, y))
	if err != nil {
		t.Fatalf("a zero-padded coordinate was refused: %v", err)
	}
	if !got.(*ecdsa.PublicKey).Equal(&key.PublicKey) {
		t.Fatal("zero-padded coordinates parsed to a different key")
	}
}

func TestAnECJWKThatIsNotAValidKeyIsRefused(t *testing.T) {
	c := ecCurves[0]
	key, err := ecdsa.GenerateKey(c.curve, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	x, y := padded(key.X, c.size), padded(key.Y, c.size)
	flipped := append([]byte(nil), y...)
	flipped[len(flipped)-1] ^= 0x01

	pPlusX := new(big.Int).Add(key.X, c.curve.Params().P)

	cases := []struct {
		name string
		raw  json.RawMessage
	}{
		{"a point not on the curve", ecJWK("P-256", x, flipped)},
		{"the point (0, 0)", ecJWK("P-256", make([]byte, c.size), make([]byte, c.size))},
		{"empty coordinates", ecJWK("P-256", nil, nil)},
		{"x alone empty", ecJWK("P-256", nil, y)},
		{"a coordinate whose value does not fit the curve's size", ecJWK("P-256", append([]byte{1}, x...), y)},
		{"x + p, the same point modulo p", ecJWK("P-256", pPlusX.Bytes(), y)},
		{"a P-256 point labelled P-384", ecJWK("P-384", x, y)},
		{"an unsupported curve", ecJWK("P-224", x, y)},
		{"an unknown curve", ecJWK("secp256k1", x, y)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, _, err := parseJWK(tc.raw); err == nil {
				t.Fatalf("was accepted as %#v", got)
			}
		})
	}
}

// The refusal for an off-curve point names the curve, which is what an operator reading the log needs.
func TestAnOffCurveECPointSaysSo(t *testing.T) {
	c := ecCurves[0]
	key, _ := ecdsa.GenerateKey(c.curve, rand.Reader)
	y := padded(key.Y, c.size)
	y[len(y)-1] ^= 0x01
	_, _, err := parseJWK(ecJWK("P-256", padded(key.X, c.size), y))
	if err == nil || !strings.Contains(err.Error(), "not on P-256") {
		t.Fatalf("err = %v, want it to say the point is not on P-256", err)
	}
}
