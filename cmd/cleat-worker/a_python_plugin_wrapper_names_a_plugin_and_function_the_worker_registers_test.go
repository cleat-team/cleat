package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

// cleat#3789: the typed plugin wrappers in the Python, Java and AssemblyScript
// SDKs (`Plugins.await_webhook` and its siblings) name a plugin and a function
// by string, and nothing checked those strings against the worker.
//
// `Plugins.await_webhook` called "webhookingest"; the worker registers
// "webhook-ingest". The call built, deployed and ran, and failed inside the
// guest with `plugin function webhookingest/await_webhook not registered`,
// after the workflow had already slept and taken a signal. Counting every
// wrapper call against the registry then showed it was not one typo: the
// plugin names had been written with the hyphens dropped.
//
// This reads the registry the worker itself builds (plugin.Discover plus each
// plugin's RegisterHostFunctions, recorded rather than run), so a plugin added,
// renamed or unlinked changes the answer without anyone editing this file.

type recordingRegistry struct{ names []string }

func (r *recordingRegistry) Register(opts plugin.FuncOptions, _ plugin.PluginFunc) error {
	r.names = append(r.names, opts.Name)
	return nil
}

// notLinkedOnPurpose names plugins a wrapper may call although the worker does
// not link them. A grant like this covers one thing and says why.
var notLinkedOnPurpose = map[string]string{
	"pgvector": "plugins/pgvector exists but is deliberately not imported by cmd/cleat-worker/main.go " +
		"(its migration needs the pgvector extension; see the comment there), so its wrappers can only " +
		"work on a worker built with that import added",
}

func registeredPluginFunctions(t *testing.T) map[string]map[string]bool {
	t.Helper()
	loaded, err := plugin.Discover()
	if err != nil {
		t.Fatalf("plugin.Discover: %v", err)
	}
	out := map[string]map[string]bool{}
	for _, lp := range loaded {
		name := lp.Plugin.Info().Name
		out[name] = map[string]bool{}
		hf, ok := lp.Plugin.(plugin.HasHostFunctions)
		if !ok {
			continue
		}
		rec := &recordingRegistry{}
		if err := hf.RegisterHostFunctions(rec); err != nil {
			t.Fatalf("%s: RegisterHostFunctions: %v", name, err)
		}
		for _, fn := range rec.names {
			out[name][fn] = true
		}
	}
	return out
}

// pluginWrapperSources are the three SDK files that wrap plugin calls in typed
// methods. The Java and AssemblyScript wrappers were written beside the Python
// one and carry the same names, so the same defect: all three are read.
var pluginWrapperSources = []struct {
	file     string
	call     *regexp.Regexp
	minCalls int // the extractor's own control; see the test
}{
	{filepath.Join("python-sdk", "cleat_sdk", "plugins.py"),
		regexp.MustCompile(`self\._call\(\s*self\._h,\s*"([^"]+)",\s*"([^"]+)"`), 13},
	{filepath.Join("crates", "cleat-java", "src", "main", "java", "cleat", "Plugins.java"),
		regexp.MustCompile(`host\.pluginCall\("([^"]+)", "([^"]+)"`), 12},
	{filepath.Join("packages", "cleat-as", "assembly", "plugins.ts"),
		regexp.MustCompile(`this\.host\.pluginCall\("([^"]+)", "([^"]+)"`), 13},
}

func TestEveryPluginWrapperNamesAPluginAndFunctionTheWorkerRegisters(t *testing.T) {
	registered := registeredPluginFunctions(t)
	// The registry must be able to say yes: a known pair has to be in it.
	if !registered["webhook-ingest"]["await_webhook"] {
		t.Fatalf("the registry does not contain webhook-ingest/await_webhook, so it cannot vouch for anything: %v", registered)
	}
	var known []string
	for n := range registered {
		known = append(known, n)
	}
	sort.Strings(known)

	for _, src := range pluginWrapperSources {
		t.Run(src.file, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join("..", "..", src.file))
			if err != nil {
				t.Fatal(err)
			}
			calls := src.call.FindAllSubmatch(body, -1)
			// The extractor must be able to see the calls it is about to judge:
			// a regexp that stopped matching would turn every assertion below
			// into a pass.
			if len(calls) < src.minCalls {
				t.Fatalf("found %d wrapper calls in %s; there are %d, so the extractor has stopped matching",
					len(calls), src.file, src.minCalls)
			}
			seen := map[string]bool{}
			var bad []string
			for _, c := range calls {
				pl, fn := string(c[1]), string(c[2])
				if seen[pl+"/"+fn] {
					continue
				}
				seen[pl+"/"+fn] = true
				if _, exempt := notLinkedOnPurpose[pl]; exempt {
					continue
				}
				funcs, linked := registered[pl]
				switch {
				case !linked:
					bad = append(bad, pl+"/"+fn+"  (no linked plugin of that name)")
				case !funcs[fn]:
					bad = append(bad, pl+"/"+fn+"  (plugin is linked; it registers no function of that name)")
				}
			}
			sort.Strings(bad)
			if len(bad) > 0 {
				t.Errorf("%s has %d wrapper call(s) the worker cannot resolve:\n  %s\n\nlinked plugins: %v",
					src.file, len(bad), joinLines(bad), known)
			}
		})
	}
}

func joinLines(s []string) string {
	out := ""
	for i, l := range s {
		if i > 0 {
			out += "\n  "
		}
		out += l
	}
	return out
}
