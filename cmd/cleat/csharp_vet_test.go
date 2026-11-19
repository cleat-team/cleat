package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The C# determinism vet (cleat#4080, PR 6). The rules are in vet_csharp.go with the measurement each one cites; these tests hold the
// vet to them: a known positive for EVERY rule (a vet that returns nothing for every input also "passes" the corpus), the APIs the probe
// measured FINE are not refused, names are resolved rather than matched, prose is not code, and the exit statuses keep 0, 1 and 2 apart.

func vetCS(src string) []csFinding { return findForbiddenCSharp(csharpCodeOnly([]byte(src))) }

func codesOf(fs []csFinding) string {
	var c []string
	for _, f := range fs {
		c = append(c, f.code)
	}
	sort.Strings(c)
	return strings.Join(c, ",")
}

// positives has one source per code the vet can emit. TestEveryCodeHasAKnownPositive fails if a rule is added without one.
var csharpPositives = map[string]string{
	"CV001": `var t = Task.Run(() => 1);`,
	"CV002": `await Task.Delay(10);`,
	"CV003": `var done = pending.Wait();`,
	"CV004": `var t = new Thread(() => { });`,
	"CV005": `Thread.Sleep(50);`,
	"CV006": `var sw = Stopwatch.StartNew();`,
	"CV007": `var ms = Environment.TickCount64;`,
	"CV008": `var n = RandomNumberGenerator.GetInt32(10);`,
	"CV009": `var s = File.ReadAllText("/etc/hostname");`,
	"CV010": `var c = new HttpClient();`,
}

func TestEveryCodeHasAKnownPositive(t *testing.T) {
	codes := map[string]bool{"CV003": true, "CV004": true} // both are matched outside the table
	for _, r := range csharpRules {
		codes[r.code] = true
	}
	for code := range codes {
		src, ok := csharpPositives[code]
		if !ok {
			t.Errorf("%s has no known positive in csharpPositives: a rule nothing has been shown to trip is a claim", code)
			continue
		}
		if got := codesOf(vetCS(src)); got != code {
			t.Errorf("%s: %q produced %q, want exactly %s", code, src, got, code)
		}
	}
	for code := range csharpPositives {
		if !codes[code] {
			t.Errorf("csharpPositives names %s, which no rule emits", code)
		}
	}
}

// Every API the probe measured FINE on the real engine is left alone. This is the half of the table that stops the vet refusing by name.
func TestMeasuredFineAPIsAreNotRefused(t *testing.T) {
	fine := `
using System;
using System.Threading.Tasks;
public static class W {
    [CleatEntry("w")]
    public static async Task<JsonNode> Run(HostCalls h, JsonElement input) {
        var a = DateTime.UtcNow; var b = DateTime.Now; var c = DateTimeOffset.UtcNow; var tz = TimeZoneInfo.Local;
        var r1 = Random.Shared.NextInt64(); var r2 = new Random().Next(); var r3 = new Random(42).Next();
        var g = Guid.NewGuid(); var hc = "abc".GetHashCode(); var oh = RuntimeHelpers.GetHashCode(new object());
        await Task.Yield(); var done = await Task.FromResult(1); var both = await Task.WhenAll(Task.FromResult(1), Task.FromResult(2));
        Console.WriteLine("x"); var m = Environment.MachineName; var p = Environment.ProcessId;
        var first = h.Call("svc", "op", "{}"); h.Sleep(TimeSpan.FromSeconds(1)); var now = h.Now(); var rnd = h.Random();
        try { h.Sleep(TimeSpan.FromSeconds(60)); } catch (Exception) { }
        return new JsonObject();
    }
}`
	if fs := vetCS(fine); len(fs) != 0 {
		t.Fatalf("APIs measured fine were refused: %+v", fs)
	}
	// Negative control: the same file with ONE violation reports exactly that violation, so the empty result above was not the vet
	// failing to see anything.
	fs := vetCS(strings.Replace(fine, "await Task.Yield();", "await Task.Yield(); Thread.Sleep(1);", 1))
	if codesOf(fs) != "CV005" {
		t.Fatalf("one added Thread.Sleep gave %q, want CV005 alone", codesOf(fs))
	}
}

// Prose is not code. Each form names a refused API without using it.
func TestCSharpVetReadsCodeNotProse(t *testing.T) {
	prose := map[string]string{
		"line comment":         "var x = 1; // Task.Run(() => 1)",
		"doc comment":          "/// <summary>do not call Thread.Sleep(5)</summary>\nvar x = 1;",
		"block comment":        "/*\n  Stopwatch.StartNew() described here\n*/\nvar x = 1;",
		"string":               `var s = "File.ReadAllText(x)";`,
		"verbatim string":      "var s = @\"a \"\"Task.Run\"\" b \\ Thread.Sleep(1)\";",
		"interpolated literal": `var s = $"call Task.Delay(1) then {x}";`,
		"raw string":           "var s = \"\"\"\n  Environment.TickCount64\n  \"\"\";",
		"char literals":        `var a = '"'; var b = '\''; var c = 'x';`,
		"escaped quote":        `var s = "a \" Task.Run(1) b";`,
	}
	for name, src := range prose {
		if fs := vetCS(src); len(fs) != 0 {
			t.Errorf("%s: the vet read prose as code: %+v", name, fs)
		}
	}
	// Code inside an interpolation hole IS code.
	if got := codesOf(vetCS(`var s = $"value {Task.Run(() => 1).Result} and {{literal Thread.Sleep(1)}}";`)); got != "CV001" {
		t.Errorf("a call inside an interpolation hole gave %q, want CV001 (and not the escaped-brace text)", got)
	}
	// A string that follows a comment on the same line still opens: the blanker must not stay inside the comment.
	if got := codesOf(vetCS("var a = 1; /* c */ Thread.Sleep(1); var s = \"x\";")); got != "CV005" {
		t.Errorf("code after a block comment gave %q, want CV005", got)
	}
}

// The blanker must keep every offset and line ending, since a finding's line and column are read straight out of its result.
func TestCSharpCodeOnlyKeepsOffsets(t *testing.T) {
	src := "var a = \"x\\n\"; // c\r\n/* b\n */ var s = $\"{y}\" + @\"q\"\"r\";\n"
	out := csharpCodeOnly([]byte(src))
	if len(out) != len(src) {
		t.Fatalf("length changed: %d -> %d", len(src), len(out))
	}
	for i := range src {
		if (src[i] == '\n' || src[i] == '\r') && out[i] != src[i] {
			t.Fatalf("a line ending at byte %d was changed", i)
		}
	}
}

// Names are resolved, not matched as substrings.
func TestCSharpVetResolvesNames(t *testing.T) {
	cases := []struct {
		name, src, want string
	}{
		{"fully qualified", `var s = System.IO.File.ReadAllText(p);`, "CV009"},
		{"global-qualified", `global::System.Threading.Thread.Sleep(1);`, "CV005"},
		{"qualified with the wrong namespace", `var s = My.IO.File.ReadAllText(p);`, ""},
		{"aliased type", "using T = System.Threading.Tasks.Task;\nvar t = T.Run(() => 1);", "CV001"},
		{"using static", "using static System.Threading.Tasks.Task;\nvar t = Run(() => 1);\nawait Delay(5);", "CV001,CV002"},
		{"using static does not reach a member call", "using static System.Threading.Tasks.Task;\nvar t = other.Run(1);", ""},
		{"a type this file declares", "class File { public static bool Exists(string p) => true; }\nvar b = File.Exists(\"x\");", ""},
		{"an alias to somewhere else", "using File = MyLib.File;\nvar b = File.Exists(\"x\");", ""},
		{"a member access on a value", `var b = this.File.Exists(x); var c = svc.Directory.GetFiles(y);`, ""},
		{"a member access on a value, deeper", `var t = a.b.Task.Run(1);`, ""},
		{"a member of a call result", `var t = Make().File.Exists(x);`, ""},
		{"a method named like a type is not the type", `var t = Env.Stopwatch(x);`, ""},
		{"Task<T> alone is not a refusal", `Task<int> t = Compute(); var u = await t;`, ""},
		{"a bare Thread is not a refusal", `Thread t = null;`, ""},
		{"new Thread qualified", `var t = new System.Threading.Thread(Work);`, "CV004"},
		{"new Thread of another namespace", `var t = new My.Thread(Work);`, ""},
		{"Task.Wait with a timeout", `pending.Wait(TimeSpan.FromSeconds(1));`, "CV003"},
		{"WaitAsync is not Wait", `await pending.WaitAsync(ct);`, ""},
	}
	for _, c := range cases {
		if got := codesOf(vetCS(c.src)); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// A finding carries a usable position.
func TestCSharpVetReportsTheLineAndColumn(t *testing.T) {
	fs := vetCS("var a = 1;\n    var b = Thread.Sleep(5);\n")
	if len(fs) != 1 || fs[0].line != 2 || fs[0].col != 13 {
		t.Fatalf("got %+v, want one finding at 2:13", fs)
	}
}

func writeCSProject(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const goodCS = "using Cleat;\npublic static class W { [CleatEntry(\"w\")] public static string Run(HostCalls h, string i) => h.WorkflowId(); }\n"
const badCS = "using Cleat;\npublic static class W {\n  [CleatEntry(\"w\")]\n  public static string Run(HostCalls h, string i) { Thread.Sleep(5); return \"\"; }\n}\n"

// 0, 1 and 2 are three different answers, and a vet that read nothing must not say 0.
func TestRunVetCSharpKeepsThreeStatusesApart(t *testing.T) {
	var out bytes.Buffer
	if rc := runVetCSharp(writeCSProject(t, map[string]string{"W.cs": goodCS, "W.csproj": "<Project/>"}), false, &out, &out); rc != 0 {
		t.Errorf("a clean project: rc=%d, want 0\n%s", rc, out.String())
	}
	out.Reset()
	rc := runVetCSharp(writeCSProject(t, map[string]string{"W.cs": badCS, "W.csproj": "<Project/>"}), false, &out, &out)
	if rc != 1 || !strings.Contains(out.String(), "[CV005]") || !strings.Contains(out.String(), "W.cs:4:") {
		t.Errorf("a violating project: rc=%d, want 1 naming CV005 at W.cs:4\n%s", rc, out.String())
	}
	// UNMEASURED: nothing to read. Both are 2, not 0 and not 1.
	if rc := runVetCSharp(writeCSProject(t, map[string]string{"W.csproj": "<Project/>"}), false, &out, &out); rc != 2 {
		t.Errorf("a project with no .cs file: rc=%d, want 2 (UNMEASURED)", rc)
	}
	if rc := runVetCSharp(filepath.Join(t.TempDir(), "missing"), false, &out, &out); rc != 2 {
		t.Errorf("a directory that does not exist: rc=%d, want 2 (UNMEASURED)", rc)
	}
	// bin and obj hold generated sources that are not the author's.
	obj := writeCSProject(t, map[string]string{"W.cs": goodCS, "obj/Generated.cs": badCS})
	if rc := runVetCSharp(obj, false, &out, &out); rc != 0 {
		t.Errorf("a violation under obj/ was read: rc=%d, want 0", rc)
	}
}

func TestRunVetCSharpJSON(t *testing.T) {
	var out, diag bytes.Buffer
	rc := runVetCSharp(writeCSProject(t, map[string]string{"W.cs": badCS}), true, &out, &diag)
	var got VetOutput
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("the JSON did not parse: %v\n%s", err, out.String())
	}
	if rc != 1 || len(got.Errors) != 1 || got.Errors[0].Code != "CV005" || got.Errors[0].Line != 4 {
		t.Errorf("rc=%d errors=%+v, want rc 1 and one CV005 at line 4", rc, got.Errors)
	}
}

func TestCSharpVetWarnsWhenThereIsNoEntry(t *testing.T) {
	var out bytes.Buffer
	if rc := runVetCSharp(writeCSProject(t, map[string]string{"W.cs": "public static class W { }\n"}), false, &out, &out); rc != 0 || !strings.Contains(out.String(), "CV100") {
		t.Errorf("rc=%d, want 0 with a CV100 warning:\n%s", rc, out.String())
	}
}

// THE GATE. `cleat build --target csharp` refuses a violating project BEFORE it looks for the toolchain. CLEAT_CSHARP_DIR points at a
// directory with no build.sh, so a build that got past the vet would stop at the toolchain with status 2: a refusal at status 1 that says
// "determinism check" can only have come from the gate. The control proves the setup does reach the toolchain: the same environment with a
// clean project gives the toolchain's 2.
func TestBuildCSharpRefusesBeforeTheToolchain(t *testing.T) {
	t.Setenv("CLEAT_CSHARP_DIR", t.TempDir())
	run := func(files map[string]string) (int, string, string) {
		dir := writeCSProject(t, files)
		outDir := filepath.Join(t.TempDir(), "out")
		var stderr bytes.Buffer
		rc := csharpBuild(dir, outDir, 1, &stderr)
		if _, err := os.Stat(outDir); err == nil {
			ents, _ := os.ReadDir(outDir)
			if len(ents) != 0 {
				t.Errorf("an artifact was written: %v", ents)
			}
		}
		return rc, stderr.String(), dir
	}

	rc, log, _ := run(map[string]string{"W.csproj": "<Project/>", "W.cs": badCS})
	if rc != 1 || !strings.Contains(log, "determinism check failed") || strings.Contains(log, "toolchain") {
		t.Errorf("a violating project: rc=%d, want 1 from the gate, before the toolchain\n%s", rc, log)
	}

	rc, log, _ = run(map[string]string{"W.csproj": "<Project/>", "W.cs": goodCS})
	if rc != 2 || !strings.Contains(log, "UNMEASURED") || strings.Contains(log, "determinism check failed") {
		t.Errorf("control, a clean project: rc=%d, want 2 from the toolchain lookup (the gate passed)\n%s", rc, log)
	}

	// The vet could not read anything: UNMEASURED, status 2, not a pass that goes on to build.
	rc, log, _ = run(map[string]string{"W.csproj": "<Project/>"})
	if rc != 2 || !strings.Contains(log, "no .cs source file") {
		t.Errorf("a project with no sources: rc=%d, want 2 naming the missing sources\n%s", rc, log)
	}
}

func TestDetectVetLangSeesACSharpProject(t *testing.T) {
	if got, err := detectVetLang(writeCSProject(t, map[string]string{"W.csproj": "<Project/>", "W.cs": goodCS})); err != nil || got != "csharp" {
		t.Errorf("a directory with one .csproj: got %q, %v, want csharp", got, err)
	}
}

// THE CORPUS. The workflow projects in this repository must vet clean, and the probe, which deliberately uses every refused API, must not:
// without the second half a vet that reads nothing would pass the first.
func TestCSharpVetCorpus(t *testing.T) {
	root := filepath.Join("..", "..", "packages", "cleat-csharp")
	for _, p := range []string{"conformance", "fixture", "fixture-typed", "src"} {
		var out, diag bytes.Buffer
		rc := runVetCSharp(filepath.Join(root, p), true, &out, &diag)
		var res VetOutput
		if err := json.Unmarshal(out.Bytes(), &res); err != nil {
			t.Fatalf("%s: the JSON did not parse: %v", p, err)
		}
		if rc != 0 || len(res.Warnings) != 0 {
			t.Errorf("%s does not vet clean (rc=%d, warnings=%+v):\n%s", p, rc, res.Warnings, diag.String())
		}
	}
	var out, diag bytes.Buffer
	if rc := runVetCSharp(filepath.Join(root, "probe"), true, &out, &diag); rc != 1 {
		t.Fatalf("the probe vetted rc=%d, want 1: it uses every refused API on purpose\n%s", rc, out.String())
	}
	var got VetOutput
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	// The probe's four swallow entries: the two that let a call after the catch reach the service are warned about, the filtered and the
	// rethrown ones (which made none) are not.
	var warned []int
	for _, w := range got.Warnings {
		if w.Code == "CV101" {
			warned = append(warned, w.Line)
		}
	}
	if len(warned) != 2 {
		t.Errorf("the probe has two swallowing catches and the vet warned at lines %v", warned)
	} else if probe, err := os.ReadFile(filepath.Join(root, "probe", "Probe.cs")); err == nil {
		lines := strings.Split(string(probe), "\n")
		for _, l := range warned {
			if txt := lines[l-1]; !strings.Contains(txt, "catch (Exception) { }") && !strings.Contains(txt, "catch { }") {
				t.Errorf("a CV101 warning at Probe.cs:%d points at %q, not at a swallowing catch", l, strings.TrimSpace(txt))
			}
		}
	}
	seen := map[string]bool{}
	for _, e := range got.Errors {
		seen[e.Code] = true
	}
	// Every code the probe has an entry for. CV010 is absent on purpose: HttpClient cannot be in the probe (see Probe.cs).
	for _, code := range []string{"CV001", "CV002", "CV003", "CV004", "CV005", "CV006", "CV007", "CV008", "CV009"} {
		if !seen[code] {
			t.Errorf("the probe was not refused with %s; it reached %v", code, seen)
		}
	}
}

// CV101, the warning: a catch that can swallow CleatSuspendException. The four cases are the probe's four swallow entries
// (packages/cleat-csharp/probe, measured in tests/csharp/determinism_probe_test.go): the first two let a host call after the catch reach the
// service, and the filtered and rethrown forms did not.
func TestTheSwallowingCatchWarningMatchesTheProbe(t *testing.T) {
	warn := func(src string) int { return len(findSwallowingCatches(csharpCodeOnly([]byte(src)))) }
	cases := []struct {
		name, src string
		want      int
	}{
		{"catch (Exception) {}", "try { h.Sleep(d); } catch (Exception) { }", 1},
		{"bare catch {}", "try { h.Sleep(d); } catch { }", 1},
		{"catch (Exception e) that only logs", "try { x(); } catch (Exception e) { log(e); }", 1},
		{"System.Exception spelled out", "try { x(); } catch (System.Exception) { }", 1},
		{"filtered to exclude the suspension", "try { x(); } catch (Exception e) when (e is not CleatSuspendException) { }", 0},
		{"rethrown", "try { x(); } catch (Exception) { throw; }", 0},
		{"rethrown after logging", "try { x(); } catch (Exception e) { log(e); throw; }", 0},
		{"the suspension is handled first", "try { x(); } catch (CleatSuspendException) { throw; } catch (Exception) { }", 0},
		{"the suspension handler does NOT rethrow, so the next catch is not safe", "try { x(); } catch (CleatSuspendException) { } catch (Exception) { }", 1},
		{"a helper filter with suspend in its name", "try { x(); } catch (Exception e) when (ExceptionInfo.IsSuspend(e)) { return; } catch (Exception) { }", 0},
		{"the suspension handler returns", "try { x(); } catch (CleatSuspendException) { return Outcome.Suspended(); } catch (Exception) { }", 0},
		{"a narrower exception", "try { x(); } catch (ArgumentException) { }", 0},
		{"in a comment", "// try { x(); } catch (Exception) { }", 0},
		{"in a string", `var s = "catch (Exception) { }";`, 0},
		{"two swallowing catches", "try { a(); } catch { }\ntry { b(); } catch (Exception) { }", 2},
		{"a nested block in the body", "try { x(); } catch (Exception) { if (y) { z(); } }", 1},
		// A try nested in a catch body: the inner catch starts BEFORE the outer catch's body ends. This crashed the first version of the
		// detector with a slice out of range, found by running it over the repository's own C#.
		{"a try nested in a catch body", "try { a(); } catch (ArgumentException) { try { b(); } catch { } } catch (Exception) { }", 2},
	}
	for _, c := range cases {
		if got := warn(c.src); got != c.want {
			t.Errorf("%s: %d warning(s), want %d", c.name, got, c.want)
		}
	}
}

// A warning does not fail the build: the project below swallows and still vets 0, and the warning is reported.
func TestTheSwallowingCatchIsAWarningAndDoesNotFailTheVet(t *testing.T) {
	var out bytes.Buffer
	src := "public static class W { [CleatEntry(\"w\")] public static string Run(HostCalls h, string i) { try { h.Sleep(d); } catch (Exception) { } return \"\"; } }\n"
	if rc := runVetCSharp(writeCSProject(t, map[string]string{"W.cs": src}), false, &out, &out); rc != 0 || !strings.Contains(out.String(), "Warning [CV101] W.cs:1:") {
		t.Errorf("rc=%d, want 0 with a CV101 warning at W.cs:1\n%s", rc, out.String())
	}
}
