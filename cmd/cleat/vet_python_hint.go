package main

import (
	"fmt"
	"os/exec"
	"strings"
)

// pythonSDKMinVersion is the interpreter the Python SDK needs. It uses PEP 604
// unions (`tuple[str, str] | None`), which are a TypeError at import time on
// anything older -- not a syntax error, so the failure arrives from deep inside
// an import chain and names a file rather than a version.
const pythonSDKMinVersion = "3.10"

// pythonVetFailureHint turns a Python traceback into a sentence about what to
// do, or returns "" when it has nothing useful to add.
//
// It exists because the traceback for the most common failure is accurate and
// unreadable. On macOS, where /usr/bin/python3 is 3.9, `cleat vet --lang python`
// ends in:
//
//	File ".../python-sdk/cleat_sdk/signal_envelope.py", line 57, in <module>
//	  def decode_signal_envelope(raw: str) -> tuple[str, str] | None:
//	TypeError: unsupported operand type(s) for |: 'types.GenericAlias' and 'NoneType'
//
// which names a line in the SDK and says nothing about interpreters. Every part
// of that is true and none of it is the answer.
//
// Deliberately a HINT appended to the real output rather than a replacement for
// it. A guessed diagnosis presented as the whole story is what made this failure
// take two people and two machines to identify -- the test's own message named
// one cause and hedged the rest, so two different environments printed the same
// sentence. The traceback is always shown; this is added beside it.
func pythonVetFailureHint(stderr string) string {
	if !strings.Contains(stderr, "unsupported operand type(s) for |") &&
		!strings.Contains(stderr, "ModuleNotFoundError: No module named 'cleat_sdk'") {
		return ""
	}

	var b strings.Builder
	if strings.Contains(stderr, "unsupported operand type(s) for |") {
		fmt.Fprintf(&b, "\nHint: the Python SDK requires Python >= %s, and the interpreter on PATH is %s.\n",
			pythonSDKMinVersion, pythonVersionOnPath())
		fmt.Fprintf(&b, "      The error above comes from a PEP 604 union (`X | None`) in the SDK,\n")
		fmt.Fprintf(&b, "      which is a TypeError at import time on older interpreters.\n")
		fmt.Fprintf(&b, "      Put a newer python3 first on PATH, e.g. `brew install python@3.12`.\n")
		return b.String()
	}
	fmt.Fprintf(&b, "\nHint: cleat_sdk was not importable. Set PYTHONPATH to the SDK directory,\n")
	fmt.Fprintf(&b, "      e.g. PYTHONPATH=./python-sdk, or install it with `pip install -e python-sdk`.\n")
	return b.String()
}

// pythonVersionOnPath reports the version of the python3 that would be run, or
// a placeholder when it cannot be determined.
//
// Best effort on purpose: this is only ever called while composing an error
// message, so a failure here must not replace the failure being reported.
func pythonVersionOnPath() string {
	out, err := exec.Command("python3", "-c",
		"import sys; print('%d.%d.%d' % sys.version_info[:3])").Output()
	if err != nil {
		return "not determinable"
	}
	return strings.TrimSpace(string(out))
}
