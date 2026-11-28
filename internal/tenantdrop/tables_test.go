package tenantdrop

import "testing"

// Moved from cmd/cleatctl with the function (cleat#2487 wrote it there).
func TestBracketQuote(t *testing.T) {
	cases := []struct{ in, want string }{
		{"tenants", "[tenants]"},
		{"weird]name", "[weird]]name]"},
		{"", "[]"},
	}
	for _, c := range cases {
		if got := BracketQuote(c.in); got != c.want {
			t.Errorf("BracketQuote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
