package engine

import (
	"strings"
	"testing"
	"unicode/utf16"
)

// nvarchar(255) counts UTF-16 code units, and a character outside the Basic Multilingual
// Plane is two of them. The first version of the cut was by rune, so 128 such characters
// (256 units) failed the whole install on SQL Server (Msg 2628, found on review of
// cleat#3599 on a real SQL Server with nvarchar(3) and two emoji).
func TestInstalledByIsCutToTheColumnsWidthInUTF16Units(t *testing.T) {
	units := func(s string) int { return len(utf16.Encode([]rune(s))) }
	for _, tc := range []struct {
		name string
		in   string
		max  int
		want int // wanted length in UTF-16 units
	}{
		{"short ASCII is untouched", "alice", 255, 5},
		{"long ASCII is cut to the width", strings.Repeat("a", 300), 255, 255},
		{"astral characters count two units each", strings.Repeat("\U0001F600", 200), 255, 254},
		{"a cut never splits a surrogate pair", "ab" + strings.Repeat("\U0001F600", 5), 5, 4},
		{"mixed", strings.Repeat("a\U0001F600", 100), 255, 255},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateUTF16(tc.in, tc.max)
			if units(got) != tc.want {
				t.Errorf("cut to %d units, want %d", units(got), tc.want)
			}
			if units(got) > tc.max {
				t.Errorf("%d units is over the width %d", units(got), tc.max)
			}
			if !strings.HasPrefix(tc.in, got) {
				t.Errorf("the cut value is not a prefix of the input")
			}
		})
	}
}
