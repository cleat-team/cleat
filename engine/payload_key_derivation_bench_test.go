package engine

import "testing"

// The measurement behind deriving once per EVENT rather than once per field.
//
// Committed rather than left in a scratch directory because the ratio in
// tenantCipher's doc comment is otherwise a number with no way to re-derive it,
// and CLAUDE.md's rule is that a number without its command does not get
// written down. THIS FILE AND tenantCipher ARE THE ONLY TWO PLACES THE RATIO
// APPEARS: I first repeated it in four, corrected three of them after the
// measurement moved from 1.10x to 1.09x, and missed one -- which is the whole
// argument for not copying a number around.
//
//	go test ./engine/ -run '^$' -bench BenchmarkPayloadSeal -benchtime 2s -count=5
//
// Medians on an M-series laptop, 2026-09-17, min/max within 1%:
//
//	NoDerive          5997 ns/op   1.00x   (the code before cleat#1793)
//	PerEventDerive    6537 ns/op   1.09x
//	PerFieldDerive   12261 ns/op   2.04x
//
// A -benchtime of 500x gave 1.36x for PerEventDerive, which is noise: one
// derivation cannot cost four seals. Use 2s or longer before believing a ratio
// from this file.
//
// Absolute numbers are machine-dependent and will drift; the RATIO is the point
// and it follows from one derivation costing about one seal (530 vs 540 ns).

// fieldsPerEvent is the upper bound, not the mean: encodeEventForStorage seals
// ten string columns plus payload, and an ordinary `call` event leaves seven of
// the ten empty. Benchmarking the worst case is deliberate, and saying so is
// what stops the ratio above being read as a typical cost.
const fieldsPerEvent = 11

var benchField = []byte(`{"card":"4111111111111111","amount":1299}`)

func benchCipher(b *testing.B) (*PayloadEncryption, *tenantCipher) {
	b.Helper()
	pe, err := NewPayloadEncryption("MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	if err != nil {
		b.Fatalf("NewPayloadEncryption: %v", err)
	}
	tc, err := pe.forTenant("11111111-1111-1111-1111-111111111111")
	if err != nil {
		b.Fatalf("forTenant: %v", err)
	}
	return pe, tc
}

func BenchmarkPayloadSealPerEventDerive(b *testing.B) {
	pe, _ := benchCipher(b)
	for i := 0; i < b.N; i++ {
		tc, err := pe.forTenant("11111111-1111-1111-1111-111111111111")
		if err != nil {
			b.Fatal(err)
		}
		for f := 0; f < fieldsPerEvent; f++ {
			if _, err := tc.seal(benchField); err != nil {
				b.Fatal(err)
			}
		}
	}
}

func BenchmarkPayloadSealPerFieldDerive(b *testing.B) {
	pe, _ := benchCipher(b)
	for i := 0; i < b.N; i++ {
		for f := 0; f < fieldsPerEvent; f++ {
			if _, err := pe.Encrypt("11111111-1111-1111-1111-111111111111", benchField); err != nil {
				b.Fatal(err)
			}
		}
	}
}

// The control: the same eleven seals with no derivation at all, which is what
// the code did before cleat#1793. Without it the two above are two numbers with
// nothing to be relative to.
func BenchmarkPayloadSealNoDerive(b *testing.B) {
	_, tc := benchCipher(b)
	for i := 0; i < b.N; i++ {
		for f := 0; f < fieldsPerEvent; f++ {
			if _, err := sealGCM(tc.master, benchField, []byte(tc.tenantID)); err != nil {
				b.Fatal(err)
			}
		}
	}
}
