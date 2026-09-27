package main

import (
	"fmt"
	"os"
	"strings"
)

// refuseTrailingFlags exits if a flag was written AFTER the positional
// argument, where Go's flag package silently ignores it.
//
// THE BEHAVIOUR IT REFUSES. flag.Parse stops at the first non-flag argument --
// standard, documented, and not a bug in the package. Everything after that
// lands in Args(), callers take Args()[0] as the path, and the rest is dropped
// without a word. Measured on cleat#1800, same binary, same project, only the
// argument order changed:
//
//	cleat build --target rust <proj> -o <out>    files in <out>: 0   in cwd: 1
//	cleat build --target rust -o <out> <proj>    files in <out>: 1   in cwd: 0
//
// Exit 0 both times. The artifact goes to the working directory and nothing
// says so.
//
// WHY THAT IS WORSE THAN AN IGNORED FLAG. `-o` names where the build output
// goes, so ignoring it does not degrade the run, it RELOCATES it. Two .wasm
// files landed in this repository's root during cleat#1811's example checks and
// one was swept into a commit by `git add -A` before anyone noticed; the issue
// reports the same thing writing `001.wasm` into cmd/cleat/ from a test that
// asserted it had written to t.TempDir() and passed anyway, because it asserted
// on output text rather than on where the file went.
//
// NOT FLAG REORDERING. Accepting flags after positionals means hand-rolling
// the parse or taking a dependency, and either changes how every subcommand
// reads its arguments. Refusing to be silent is the smaller claim and the one
// the issue asks for: the documented order keeps working, the other order says
// what it cannot do.
//
// A POSITIONAL MAY LEGITIMATELY BEGIN WITH "-", and this refuses it. A path
// like -weird is reachable as ./-weird, which is the same escape every other
// unix tool requires; the message says so rather than leaving the user to guess.
func refuseTrailingFlags(command string, remainder []string) {
	dropped := trailingFlags(remainder)
	if len(dropped) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "Error: %s\n", flagAfterPositionalMessage(command, remainder[0], remainder[1:], dropped))
	os.Exit(1)
}

// trailingFlags is the decision, separated from the exit so it can be tested
// without ending the test process. A predicate that can only be exercised by
// running the binary gets exercised once.
//
// A BARE "-" IS NOT A FLAG. It is the conventional spelling for stdin and is a
// positional wherever it is accepted; refusing it would turn a working
// invocation into an error, which is the opposite of this function's purpose.
func trailingFlags(remainder []string) []string {
	if len(remainder) < 2 {
		return nil
	}
	var dropped []string
	for _, a := range remainder[1:] {
		if len(a) > 1 && strings.HasPrefix(a, "-") {
			dropped = append(dropped, a)
		}
	}
	return dropped
}

// flagAfterPositionalMessage is separate so a test can assert the TEXT without
// running the binary and without exiting the test process. The message is the
// whole feature -- the behaviour being fixed is silence, so an unhelpful error
// would be a smaller version of the same defect.
func flagAfterPositionalMessage(command, positional string, rest, dropped []string) string {
	// THE SUGGESTION MOVES THE POSITIONAL TO THE END, rather than re-emitting
	// the flags it recognised. The first version printed the dropped flag
	// NAMES followed by the path -- "cleat build -o <path>" -- which silently
	// lost -o's VALUE and would have set the output directory to the project
	// path. Advice that is wrong in a new way, in the message whose whole job
	// is to stop the tool being unhelpfully silent.
	//
	// Moving one element needs no knowledge of which flags take values, which
	// is the only way to be right here without a second parse.
	reordered := append(append([]string{}, rest...), positional)

	msg := fmt.Sprintf(
		"flag %s was given after the path %q and would be IGNORED, not applied.\n"+
			"  Flags must come before the path. Write:\n"+
			"      cleat %s %s\n"+
			"  rather than:\n"+
			"      cleat %s %s %s",
		strings.Join(dropped, " "), positional,
		command, strings.Join(reordered, " "),
		command, positional, strings.Join(rest, " "))

	// ONLY WHERE IT APPLIES. An earlier version appended this unconditionally
	// and produced ".//Users/.../rust-workflow" for an absolute path.
	if strings.HasPrefix(positional, "-") {
		msg += fmt.Sprintf("\n  (this path begins with \"-\"; write it ./%s if that is deliberate)", positional)
	}
	return msg
}
