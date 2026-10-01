package analyzer

import "testing"

// TestCapitalizeIdentMatchesStringsToUpperNotArithmetic pins the bug caught
// writing this file: an arithmetic 'a'-'A' offset corrupts a parameter name
// that already starts uppercase (e.g. "ID"), because it assumes the first
// byte is a lowercase ASCII letter. strings.ToUpper on the first byte alone
// leaves an already-uppercase or non-letter start unchanged, matching
// wasm package's capitalize() exactly.
func TestCapitalizeIdentMatchesStringsToUpperNotArithmetic(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"id", "Id"},
		{"ID", "ID"},
		{"userID", "UserID"},
		{"orderID", "OrderID"},
		{"x", "X"},
	} {
		if got := capitalizeIdent(tc.in); got != tc.want {
			t.Errorf("capitalizeIdent(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
