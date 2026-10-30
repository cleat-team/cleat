package engine

import (
	"errors"
	"strings"
	"testing"
)

// cleat#3629, owner decision D59: a library's members are named "<library>.<name>", and publish
// refuses anything else. The prefix is what keeps a library's names and a tenant's own names
// apart: the deploy and install refusals compare against it, and without it a tenant's own X@2
// shadows a library's X@1 (measured on PostgreSQL and SQL Server in cleat#3629).
//
// Pure validation, no database: this is the rule at the front door. The two refusals that depend
// on it have their own tests against real databases.

func prefixedBundle(library string) PublishLibraryRequest {
	return PublishLibraryRequest{Library: library, BundleVersion: "1.0.0", Members: []PublishLibraryMember{
		pubMember(library+".parent", 1, "p", map[string]int{library + ".child": 2}),
		pubMember(library+".child", 2, "c", nil),
	}}
}

func TestPublishRefusesAMemberThatDoesNotCarryTheLibrarysPrefix(t *testing.T) {
	if err := ValidatePublishLibraryRequest(prefixedBundle("clew")); err != nil {
		t.Fatalf("a bundle whose members all carry the prefix was refused: %v", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*PublishLibraryRequest)
	}{
		{"a bare name", func(r *PublishLibraryRequest) { r.Members[1].Name = "child" }},
		{"the prefix and nothing after it", func(r *PublishLibraryRequest) { r.Members[1].Name = "clew." }},
		{"another library's prefix", func(r *PublishLibraryRequest) { r.Members[1].Name = "other.child" }},
		{"a hyphen where the dot belongs", func(r *PublishLibraryRequest) { r.Members[1].Name = "clew-child" }},
		{"the prefix in the wrong case", func(r *PublishLibraryRequest) { r.Members[1].Name = "Clew.child" }},
		{"the library name alone", func(r *PublishLibraryRequest) { r.Members[1].Name = "clew" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := prefixedBundle("clew")
			tc.mutate(&r)
			// A pin to the member's old name would be refused for its own reason; keep the bundle
			// otherwise consistent so the ONLY thing wrong is the prefix.
			r.Members[0].ChildVersions = map[string]int{r.Members[1].Name: 2}
			err := ValidatePublishLibraryRequest(r)
			if !errors.Is(err, ErrLibraryPublishRefused) {
				t.Fatalf("got %v, want a publish refusal", err)
			}
			for _, want := range []string{`"clew."`, "D59"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %s, so an operator cannot tell what to rename the member to: %v", want, err)
				}
			}
		})
	}
}

// A '.' is the separator between a library name and its members, so a '.' inside a library name
// would make `a` and `a.b` claim overlapping prefixes ("a." and "a.b."), and `a.b.x` would carry
// both. No released version has libraries (v0.5.0 has no install route), so the alphabet can be
// tightened now.
func TestALibraryNameCannotContainTheSeparator(t *testing.T) {
	for _, lib := range []string{"a.b", "clew.v2", ".x", "x."} {
		if err := ValidateLibraryRef(lib, "1.0.0"); err == nil {
			t.Errorf("ValidateLibraryRef(%q) accepted a library name containing '.'", lib)
		}
	}
	for _, lib := range []string{"clew", "my_lib", "my-lib", "Lib9"} {
		if err := ValidateLibraryRef(lib, "1.0.0"); err != nil {
			t.Errorf("ValidateLibraryRef(%q) refused a valid library name: %v", lib, err)
		}
	}
}
