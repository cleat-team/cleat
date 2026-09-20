package main

import (
	"flag"
	"io"
	"strings"
	"testing"
)

// newSilentFlagSet builds a FlagSet that writes nothing, so a deliberately bad
// argument vector does not spray the test log.
func newSilentFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func TestParseFlagsAnywhere(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantOps  []string
		wantName string
		wantN    int
		wantYes  bool
	}{
		{
			name:     "flags before the operand, which already worked",
			args:     []string{"--name", "openai", "tenant-1"},
			wantOps:  []string{"tenant-1"},
			wantName: "openai",
		},
		{
			name:     "flags after the operand, which is what cleat#1933 is",
			args:     []string{"tenant-1", "--name", "openai"},
			wantOps:  []string{"tenant-1"},
			wantName: "openai",
		},
		{
			name:     "flags on both sides of the operand",
			args:     []string{"--name", "openai", "tenant-1", "--yes"},
			wantOps:  []string{"tenant-1"},
			wantName: "openai",
			wantYes:  true,
		},
		{
			// The case a "first argument not starting with -" scan gets wrong:
			// it would return "4", the VALUE of --n, as the operand.
			name:    "an int flag's value is not mistaken for the operand",
			args:    []string{"--n", "4", "my-queue"},
			wantOps: []string{"my-queue"},
			wantN:   4,
		},
		{
			name:    "same, with the flag written after the operand",
			args:    []string{"my-queue", "--n", "4"},
			wantOps: []string{"my-queue"},
			wantN:   4,
		},
		{
			name:    "no arguments at all",
			args:    nil,
			wantOps: nil,
		},
		{
			name:    "operands only",
			args:    []string{"a", "b"},
			wantOps: []string{"a", "b"},
		},
		{
			name:     "operand order is preserved across interleaved flags",
			args:     []string{"a", "--name", "x", "b", "--yes", "c"},
			wantOps:  []string{"a", "b", "c"},
			wantName: "x",
			wantYes:  true,
		},
		{
			name:     "the single-dash spelling is the same flag",
			args:     []string{"tenant-1", "-name", "openai"},
			wantOps:  []string{"tenant-1"},
			wantName: "openai",
		},
		{
			name:     "the --flag=value spelling",
			args:     []string{"tenant-1", "--name=openai"},
			wantOps:  []string{"tenant-1"},
			wantName: "openai",
		},
		{
			// Everything after "--" is an operand even when it looks like a
			// flag. Resuming the parse loop past a "--" would parse exactly the
			// arguments it was written to protect.
			name:    "a double-dash terminator protects later arguments",
			args:    []string{"tenant-1", "--", "--name", "openai"},
			wantOps: []string{"tenant-1", "--name", "openai"},
		},
		{
			name:     "a terminator does not discard the flags before it",
			args:     []string{"tenant-1", "--name", "openai", "--", "--yes"},
			wantOps:  []string{"tenant-1", "--yes"},
			wantName: "openai",
		},
		{
			// This is the case that DISCRIMINATES. With a single flag-looking
			// argument after "--", a parse loop that does not special-case the
			// terminator still lands on the right answer by accident: Parse
			// stops on "--", the loop takes the next argument as an operand,
			// and there is nothing left to mis-parse. Two arguments after the
			// terminator break that -- the loop resumes and parses "--name",
			// consuming "openai" as its value.
			//
			// Both of the cases above pass with the terminator handling
			// deleted. This one does not, which is the only reason any of them
			// is evidence.
			name:    "a terminator protects more than one following argument",
			args:    []string{"tenant-1", "--", "--yes", "--name", "openai"},
			wantOps: []string{"tenant-1", "--yes", "--name", "openai"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := newSilentFlagSet("test")
			name := fs.String("name", "", "")
			yes := fs.Bool("yes", false, "")
			n := fs.Int("n", 0, "")

			ops, err := parseFlagsAnywhere(fs, tc.args)
			if err != nil {
				t.Fatalf("parseFlagsAnywhere(%q) returned %v", tc.args, err)
			}
			if strings.Join(ops, "\x00") != strings.Join(tc.wantOps, "\x00") {
				t.Errorf("operands: got %q, want %q", ops, tc.wantOps)
			}
			if *name != tc.wantName {
				t.Errorf("--name: got %q, want %q", *name, tc.wantName)
			}
			if *yes != tc.wantYes {
				t.Errorf("--yes: got %v, want %v", *yes, tc.wantYes)
			}
			if *n != tc.wantN {
				t.Errorf("--n: got %d, want %d", *n, tc.wantN)
			}
		})
	}
}

func TestParseFlagsAnywhere_ReturnsTheParseError(t *testing.T) {
	// An unknown flag must reach the caller as an error. Swallowing it as a
	// positional is how `set-secret <uuid> --nonesuch` came to do nothing and
	// report nothing.
	for _, args := range [][]string{
		{"--nonesuch"},
		{"tenant-1", "--nonesuch"},
		{"tenant-1", "--name", "openai", "--nonesuch"},
	} {
		fs := newSilentFlagSet("test")
		fs.String("name", "", "")

		ops, err := parseFlagsAnywhere(fs, args)
		if err == nil {
			t.Errorf("parseFlagsAnywhere(%q) accepted an unknown flag, returning operands %q", args, ops)
		}
		if ops != nil {
			t.Errorf("parseFlagsAnywhere(%q) returned operands %q alongside an error; callers "+
				"that ignore the error would act on a half-parsed vector", args, ops)
		}
	}
}
