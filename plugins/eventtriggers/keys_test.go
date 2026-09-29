package eventtriggers

import "testing"

// cleat#2625. §4.3 of the design doc: never truncate a correlation key over
// the slot cap, and never accept a fourth. Both are hard errors, so a caller
// finds out at the call that is wrong instead of at every future call that
// silently never matches again.
func TestKeySlots(t *testing.T) {
	t.Run("no keys is the all-empty sentinel", func(t *testing.T) {
		k1, k2, k3, err := keySlots(nil)
		if err != nil {
			t.Fatalf("keySlots(nil): %v", err)
		}
		if k1 != "" || k2 != "" || k3 != "" {
			t.Errorf("keySlots(nil) = (%q, %q, %q), want all empty", k1, k2, k3)
		}
	})

	t.Run("keys fill left to right and leave the rest empty", func(t *testing.T) {
		k1, k2, k3, err := keySlots([]string{"A-991"})
		if err != nil {
			t.Fatalf("keySlots: %v", err)
		}
		if k1 != "A-991" || k2 != "" || k3 != "" {
			t.Errorf("keySlots([A-991]) = (%q, %q, %q), want (A-991, \"\", \"\")", k1, k2, k3)
		}
	})

	t.Run("all three slots", func(t *testing.T) {
		k1, k2, k3, err := keySlots([]string{"a", "b", "c"})
		if err != nil {
			t.Fatalf("keySlots: %v", err)
		}
		if k1 != "a" || k2 != "b" || k3 != "c" {
			t.Errorf("keySlots([a,b,c]) = (%q, %q, %q), want (a, b, c)", k1, k2, k3)
		}
	})

	t.Run("a fourth key is a hard error, not a silent drop", func(t *testing.T) {
		_, _, _, err := keySlots([]string{"a", "b", "c", "d"})
		if err == nil {
			t.Fatal("keySlots([a,b,c,d]): expected an error, got nil -- a 4th key was silently dropped")
		}
	})

	t.Run("a key over the byte cap is a hard error, not a silent truncation", func(t *testing.T) {
		over := make([]byte, maxCorrelationKeyBytes+1)
		for i := range over {
			over[i] = 'x'
		}
		_, _, _, err := keySlots([]string{string(over)})
		if err == nil {
			t.Fatal("keySlots(129-byte key): expected an error, got nil -- a key over the cap " +
				"was silently truncated, which is the exact never-matches-again bug this " +
				"function exists to prevent")
		}
	})

	t.Run("exactly at the byte cap is fine", func(t *testing.T) {
		exact := make([]byte, maxCorrelationKeyBytes)
		for i := range exact {
			exact[i] = 'x'
		}
		k1, _, _, err := keySlots([]string{string(exact)})
		if err != nil {
			t.Fatalf("keySlots(128-byte key): %v", err)
		}
		if len(k1) != maxCorrelationKeyBytes {
			t.Errorf("keySlots(128-byte key) returned a key of length %d, want %d", len(k1), maxCorrelationKeyBytes)
		}
	})
}
