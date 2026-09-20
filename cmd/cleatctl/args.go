package main

import "flag"

// parseFlagsAnywhere parses flags that appear before OR after positional
// arguments, and returns the positionals in the order they were written.
//
// # Why this exists
//
// Go's flag package stops parsing at the first argument that is not a flag.
// Every cleatctl subcommand whose argument vector begins with an operand --
// `set-secret <tenant> --name x`, `suspend-tenant <tenant> --yes` -- therefore
// received its flags unparsed and at their zero values, and then reported the
// operator's flags as missing (cleat#1933). `suspend-tenant <tenant>` worked
// and `suspend-tenant <tenant> --yes` did not, so adding the documented flag
// broke a working command.
//
// # Why not scan for the first argument that does not start with "-"
//
// Because a flag's VALUE does not start with "-" either. In
// `--concurrency 4 my-queue` such a scan returns "4" as the operand and the
// real operand as a stray. Letting Parse run until it stops, taking exactly the
// one argument it stopped on, and resuming on the remainder keeps the flag
// package's own knowledge of which flags take values, which is the only thing
// that can tell "4" from "my-queue".
//
// A parse error is returned unchanged; the FlagSet has already written its own
// message to its output, and its ErrorHandling decides whether that message was
// accompanied by usage. Callers are responsible for the exit status: returning
// the error without exiting is how an unknown flag came to be ignored silently.
func parseFlagsAnywhere(fs *flag.FlagSet, args []string) ([]string, error) {
	// Everything after a literal "--" is an operand, including anything that
	// looks like a flag. Parse honours "--" but only as a stop, so resuming the
	// loop past one would parse the arguments it was written to protect.
	head, tail := args, []string(nil)
	for i, a := range args {
		if a == "--" {
			head, tail = args[:i], args[i+1:]
			break
		}
	}

	var operands []string
	rest := head
	for {
		if err := fs.Parse(rest); err != nil {
			return nil, err
		}
		rest = fs.Args()
		if len(rest) == 0 {
			break
		}
		operands = append(operands, rest[0])
		rest = rest[1:]
	}
	return append(operands, tail...), nil
}
