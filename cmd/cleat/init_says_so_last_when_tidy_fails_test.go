package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cleat#3533, owner decision D45. `cleat init` runs `go mod tidy` in the
// scaffold, and when that fails it still exits 0, so an offline user gets a
// project. What it said about the failure used to be in the middle of its
// output, with "Created ... project" printed after it (and two more lines
// after that for fullstack), so the last thing a user read was that all was
// well. The last line of init's output is now what is wrong and the command
// that fixes it.
//
// GOPROXY=off makes the scaffold's `go mod tidy` fail deterministically, with
// the real Go toolchain and no network, which is how cleat#3533's own shape
// reproduces. Every Go template is run, because each scaffold function
// prints its own ending and the line has to come after all of them.
//
// It runs runInit in this process, the function main calls, rather than the
// built binary, so it needs no skip: a binary-based test here would have to
// skip under -short, which is a new way for it to read as a pass. stdout and
// stderr go to ONE pipe, so what is read back is in the order it was written,
// which is the claim. Reaching the assertions at all shows runInit returned:
// it would have called os.Exit, and ended the test binary, otherwise.
func TestInitEndsWithTheFixWhenTidyFailsAndStillExitsZero(t *testing.T) {
	for _, template := range []string{"basic", "agent", "agent-workflow", "workflow", "fullstack"} {
		t.Run(template, func(t *testing.T) {
			t.Setenv("GOPROXY", "off")
			t.Chdir(t.TempDir())
			name := "p_" + template
			out := captureInitOutput(t, "--template", template, name)

			if !strings.Contains(out, "could not resolve dependencies") {
				t.Fatalf("GOPROXY=off did not make the scaffold's tidy fail, so this measured nothing:\n%s", out)
			}
			lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
			last := lines[len(lines)-1]
			want := "WARNING: " + name + " was created but will not build yet"
			if !strings.HasPrefix(last, want) || !strings.HasSuffix(last, "Fix it with: cd "+name+" && go mod tidy") {
				t.Errorf("the LAST line of init's output is not the fix.\nlast line: %q\nwant it to start %q and end with the command\nfull output:\n%s", last, want, out)
			}
			if n := strings.Count(out, "WARNING: "+name); n != 1 {
				t.Errorf("the closing line appears %d times, want 1:\n%s", n, out)
			}
			if _, err := os.Stat(filepath.Join(name, "go.mod")); err != nil {
				t.Errorf("the user must still get the project, but there is no go.mod: %v", err)
			}
		})
	}
}

// captureInitOutput runs `cleat init args...` in this process and returns what
// it wrote to stdout and stderr, interleaved in write order.
func captureInitOutput(t *testing.T, args ...string) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		io.Copy(&buf, r)
		close(done)
	}()
	oldStdout, oldStderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = w, w
	func() {
		defer func() {
			os.Stdout, os.Stderr = oldStdout, oldStderr
			w.Close()
		}()
		runInit(args)
	}()
	<-done
	r.Close()
	return buf.String()
}

func TestTidyFailureReportsNothingWhenThereIsNoFailure(t *testing.T) {
	var buf bytes.Buffer
	(*tidyFailure)(nil).report(&buf)
	if buf.Len() != 0 {
		t.Errorf("a successful init's output must not change, but report wrote %q", buf.String())
	}
}

func TestTidyFailureNamesTheProjectAndQuotesADirectoryThatNeedsIt(t *testing.T) {
	for dir, want := range map[string]string{
		"myproj":        "cd myproj && go mod tidy",
		"a/b-c.d_e":     "cd a/b-c.d_e && go mod tidy",
		"my project":    "cd 'my project' && go mod tidy",
		"it's":          `cd 'it'\''s' && go mod tidy`,
		"semi;colon":    "cd 'semi;colon' && go mod tidy",
		"dollar$(boom)": "cd 'dollar$(boom)' && go mod tidy",
	} {
		var buf bytes.Buffer
		(&tidyFailure{dir: dir}).report(&buf)
		got := buf.String()
		if !strings.HasPrefix(got, "WARNING: "+dir+" was created but will not build yet") || !strings.HasSuffix(got, want+"\n") {
			t.Errorf("dir %q: report = %q, want it to end with %q", dir, got, want)
		}
		if strings.Count(got, "\n") != 1 {
			t.Errorf("dir %q: report must be exactly one line, got %q", dir, got)
		}
	}
}
