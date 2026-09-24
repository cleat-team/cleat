package auditlog

// The offline verifier's options, kinds of export and tampers, as ONE table (cleat#2047).
//
// Three rounds of review each found a combination the hand-written cases had not tried: a
// downgrade that survived both anchors, a resumed export that no option could anchor, an
// unchained record nothing bound. Cases found one at a time say nothing about the ones nobody
// thought of, so this enumerates them: every kind of export x every option set x every tamper,
// each cell with the exit code and the finding it must produce.
//
// A cell that reads `ok` on a tampered file is a LIMIT, not a pass: the verifier cannot see that
// tamper with that option set, and the docs say so. They are kept in the table on purpose. A
// reader who wants to know what to pass reads the columns that are all findings.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// The findings a cell can expect. `ok` is exit 0 with no break; every other cell is exit 1.
const (
	mOK  = "ok"
	mDG  = "DOWNGRADED"
	mGAP = "GAP"
	mAM  = "ANCHOR MISMATCH"
	mUA  = "UNCHAINED AFTER CHAINED"
	mUC  = "UNCHAINED COUNT"
	mUR  = "UNCHAINED IN A RESUMED"
	mME  = "MISSING END"
	mMS  = "MISSING START"
	mJM  = "JOIN MISMATCH"
	mIN  = "INCONCLUSIVE"
	mNA  = "n/a"
)

// matrixOptions are the columns' option sets, in the order of the rows below.
var matrixOptions = []string{
	"(none)", "--require-full", "--expect-head", "--expect-floor", "--expect-after",
	"head + floor", "head + after", "--expect-unchained", "best for the kind",
	"earlier --expect-head (seq 10)", "earlier --expect-floor (seq 2)",
}

// matrixTampers are the rows' edits. Each is made SELF-CONSISTENT the way a forger would: the
// checkpoint's count is adjusted, and where a deletion moves an end the checkpoint's end moves
// with it. The three tampers after the "checkpoint left alone" pair RE-HASH with the reference
// implementation's own row_hash (testdata/audit_chain_forge.py): the chain is unkeyed, so an
// editor can. Only an anchor on the HEAD disagrees with that; --expect-floor and --expect-after
// bind where the export starts and what it links to, not the records after.
var matrixTampers = []string{
	"none", "delete first record", "delete a middle record", "delete last two records",
	"delete middle + relabel as range", "unchained added after the chain",
	"unchained added at the front", "unchained added mid-stream",
	"delete last two, checkpoint left alone", "delete first record, checkpoint left alone",
	"delete a middle record + renumber + re-hash", "edit a record + re-hash",
	"unchained added at the front, checkpoint left alone",
}

// matrixWant[kind][option][tamper]. Read a row as "with these options, what does each tamper do".
// INCONCLUSIVE is exit 2: a head or floor anchor was given and none matched a record in the file.
var matrixWant = map[string][11][13]string{
	"full": {
		//               none  del-first del-mid del-last relabel fake-tail fake-front fake-mid del-last* del-first* rewrite edit+rehash fake-front*
		/* (none)     */ {mOK, mOK, mGAP, mOK, mOK, mUA, mOK, mUA, mME, mMS, mOK, mOK, mUC},
		/* req-full   */ {mOK, mOK, mGAP, mOK, mDG, mUA, mOK, mUA, mME, mMS, mOK, mOK, mUC},
		/* head       */ {mOK, mOK, mGAP, mAM, mDG, mUA, mOK, mUA, mME, mMS, mAM, mAM, mUC},
		/* floor      */ {mIN, mIN, mGAP, mIN, mDG, mUA, mIN, mUA, mME, mMS, mIN, mIN, mUC},
		/* after      */ {mDG, mDG, mGAP, mDG, mDG, mUA, mDG, mUA, mME, mMS, mDG, mDG, mUC},
		/* head+floor */ {mOK, mOK, mGAP, mAM, mDG, mUA, mOK, mUA, mME, mMS, mAM, mAM, mUC},
		/* head+after */ {mDG, mDG, mGAP, mDG, mDG, mUA, mDG, mUA, mME, mMS, mDG, mDG, mUC},
		/* unchained  */ {mOK, mOK, mGAP, mOK, mDG, mUA, mUC, mUA, mME, mMS, mOK, mOK, mUC},
		/* best       */ {mOK, mOK, mGAP, mAM, mDG, mUA, mUC, mUA, mME, mMS, mAM, mAM, mUC},
		/* earlier head */ {mOK, mOK, mGAP, mOK, mDG, mUA, mOK, mUA, mME, mMS, mAM, mAM, mUC},
		/* earlier floor */ {mOK, mOK, mGAP, mOK, mDG, mUA, mOK, mUA, mME, mMS, mOK, mOK, mUC},
	},
	"resumed": {
		/* (none)     */ {mOK, mOK, mGAP, mOK, mOK, mUA, mUR, mUA, mME, mMS, mOK, mOK, mUC},
		/* req-full   */ {mDG, mDG, mGAP, mDG, mDG, mUA, mUR, mUA, mME, mMS, mDG, mDG, mUC},
		/* head       */ {mDG, mDG, mGAP, mDG, mDG, mUA, mUR, mUA, mME, mMS, mDG, mDG, mUC},
		/* floor      */ {mDG, mDG, mGAP, mDG, mDG, mUA, mUR, mUA, mME, mMS, mDG, mDG, mUC},
		/* after      */ {mOK, mAM, mGAP, mOK, mDG, mUA, mUR, mUA, mME, mMS, mOK, mOK, mUC},
		/* head+floor */ {mDG, mDG, mGAP, mDG, mDG, mUA, mUR, mUA, mME, mMS, mDG, mDG, mUC},
		/* head+after */ {mOK, mAM, mGAP, mAM, mDG, mUA, mUR, mUA, mME, mMS, mAM, mAM, mUC},
		/* unchained  */ {mDG, mDG, mGAP, mDG, mDG, mUA, mUR, mUA, mME, mMS, mDG, mDG, mUC},
		/* best       */ {mOK, mAM, mGAP, mAM, mDG, mUA, mUR, mUA, mME, mMS, mAM, mAM, mUC},
		/* earlier head + after */ {mOK, mAM, mGAP, mOK, mDG, mUA, mUR, mUA, mME, mMS, mOK, mOK, mUC},
		/* earlier floor */ {mDG, mDG, mGAP, mDG, mDG, mUA, mUR, mUA, mME, mMS, mDG, mDG, mUC},
	},
	"range": {
		/* (none)     */ {mOK, mOK, mOK, mOK, mNA, mUA, mOK, mUA, mOK, mOK, mOK, mOK, mUC},
		/* req-full   */ {mDG, mDG, mDG, mDG, mNA, mUA, mDG, mUA, mDG, mDG, mDG, mDG, mUC},
		/* head       */ {mDG, mDG, mDG, mDG, mNA, mUA, mDG, mUA, mDG, mDG, mDG, mDG, mUC},
		/* floor      */ {mDG, mDG, mDG, mDG, mNA, mUA, mDG, mUA, mDG, mDG, mDG, mDG, mUC},
		/* after      */ {mDG, mDG, mDG, mDG, mNA, mUA, mDG, mUA, mDG, mDG, mDG, mDG, mUC},
		/* head+floor */ {mDG, mDG, mDG, mDG, mNA, mUA, mDG, mUA, mDG, mDG, mDG, mDG, mUC},
		/* head+after */ {mDG, mDG, mDG, mDG, mNA, mUA, mDG, mUA, mDG, mDG, mDG, mDG, mUC},
		/* unchained  */ {mDG, mDG, mDG, mDG, mNA, mUA, mDG, mUA, mDG, mDG, mDG, mDG, mUC},
		/* best       */ {mDG, mDG, mDG, mDG, mNA, mUA, mDG, mUA, mDG, mDG, mDG, mDG, mUC},
		/* earlier head */ {mDG, mDG, mDG, mDG, mNA, mUA, mDG, mUA, mDG, mDG, mDG, mDG, mUC},
		/* earlier floor */ {mDG, mDG, mDG, mDG, mNA, mUA, mDG, mUA, mDG, mDG, mDG, mDG, mUC},
	},
}

func TestTheOfflineVerifierMatrix(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		// Not a skip: this table is the guard on the offline verifier, and a run that could not
		// execute it would read as a pass. python3 is already required by the repo's own checks.
		t.Fatalf("python3 is not installed, so the reference verifier cannot run: %v", err)
	}
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		p := e.plugin()
		tenant := uuid.New()
		e.record(p, tenant, 15)
		e.legacyRow(tenant, 200)
		get := func(target string) string {
			t.Helper()
			code, body := e.get(p, tenant, target)
			if code != 200 {
				t.Fatalf("%s: %d %s", target, code, body)
			}
			return body
		}
		full := get("/audit/export")
		fullEvents, fullCP, _ := exportLines(t, full)
		// full: [legacy, seq 1..15]. Resume after seq 6, and a range over seq 4..11.
		var six *exportEvent
		for i := range fullEvents {
			if fullEvents[i].Seq != nil && *fullEvents[i].Seq == 6 {
				six = &fullEvents[i]
			}
		}
		if six == nil {
			t.Fatal("no seq 6 in the export")
		}
		resumed := get("/audit/export?cursor=" + url.QueryEscape(six.Cursor))
		ranged := get("/audit/export?from=" + url.QueryEscape(e.tsOf(tenant, 4).Format(time.RFC3339Nano)) +
			"&to=" + url.QueryEscape(e.tsOf(tenant, 11).Format(time.RFC3339Nano)))

		bodies := map[string]string{"full": full, "resumed": resumed, "range": ranged}
		head := fmt.Sprintf("%d:%s", fullCP.HeadSeq, fullCP.HeadHash)
		floor := fmt.Sprintf("%d:%s", fullCP.FloorSeq, fullCP.FloorHash)
		after := fmt.Sprintf("6:%s", *six.Hash)
		// Anchors recorded EARLIER than the export: seq 10 and seq 2 of a chain that has since grown to 15.
		var earlierHead, earlierFloor string
		for _, ev := range fullEvents {
			if ev.Seq != nil && *ev.Seq == 10 {
				earlierHead = fmt.Sprintf("10:%s", *ev.Hash)
			}
			if ev.Seq != nil && *ev.Seq == 2 {
				earlierFloor = fmt.Sprintf("2:%s", *ev.Hash)
			}
		}
		if earlierHead == "" || earlierFloor == "" {
			t.Fatal("no seq 10 / seq 2 in the export")
		}

		// A record with no seq, hash or prev_hash, and an id nothing else has: what an editor
		// with write access to the file (or the table) would invent. Cloned from the legacy row.
		var legacy string
		for _, l := range strings.Split(strings.TrimSpace(full), "\n") {
			if strings.Contains(l, `"seq":null`) {
				legacy = l
			}
		}
		var lev exportEvent
		if err := json.Unmarshal([]byte(legacy), &lev); err != nil || legacy == "" {
			t.Fatalf("no unchained row in the full export: %v", err)
		}
		forged := strings.Replace(legacy, lev.ID, uuid.NewString(), 1)

		for _, kind := range []string{"full", "resumed", "range"} {
			evs, cp, raw := exportLines(t, bodies[kind])
			var unch, ch []string
			for i, ev := range evs {
				if ev.Seq == nil {
					unch = append(unch, raw[i])
				} else {
					ch = append(ch, raw[i])
				}
			}
			if len(ch) < 6 {
				t.Fatalf("%s: %d chained records, want a few more than that to tamper with", kind, len(ch))
			}
			nUnch := len(unch)
			if kind == "resumed" && nUnch != 0 {
				t.Fatalf("a resumed export carries %d unchained records", nUnch)
			}
			opts := [][]string{
				{},
				{"--require-full"},
				{"--expect-head", head},
				{"--expect-floor", floor},
				{"--expect-after", after},
				{"--expect-head", head, "--expect-floor", floor},
				{"--expect-head", head, "--expect-after", after},
				{"--expect-unchained", fmt.Sprint(nUnch)},
				nil, // best for the kind, below
				nil, // earlier head
				{"--expect-floor", earlierFloor},
			}
			opts[9] = []string{"--expect-head", earlierHead}
			switch kind {
			case "resumed":
				opts[9] = []string{"--expect-head", earlierHead, "--expect-after", after}
				opts[8] = []string{"--expect-head", head, "--expect-after", after, "--expect-unchained", "0"}
			default:
				opts[8] = []string{"--expect-head", head, "--expect-floor", floor, "--expect-unchained", fmt.Sprint(nUnch)}
			}

			for ti, tamper := range matrixTampers {
				if matrixWant[kind][0][ti] == mNA {
					continue
				}
				u, c, cpEdit := append([]string{}, unch...), append([]string{}, ch...), map[string]any{}
				mid := len(c) / 2
				dropMid := func() { c = append(c[:mid], c[mid+1:]...) }
				switch ti {
				case 0:
				case 1: // delete the first chained record; a forger moves the start to match
					gone := exportEventOf(t, c[0])
					c = c[1:]
					switch kind {
					case "full":
						cpEdit["floor_seq"], cpEdit["floor_hash"] = *gone.Seq, *gone.Hash
					case "resumed":
						cpEdit["after_seq"] = *gone.Seq
					}
				case 2:
					dropMid()
				case 3: // delete the last two; a forger moves the head to match
					c = c[:len(c)-2]
					now := exportEventOf(t, c[len(c)-1])
					cpEdit["head_seq"], cpEdit["head_hash"] = *now.Seq, *now.Hash
				case 4:
					dropMid()
					cpEdit["from"] = "2000-01-01T00:00:00.000000Z"
				case 5:
					c = append(c, forged)
				case 6:
					u = append([]string{forged}, u...)
				case 7:
					c = append(c[:mid], append([]string{forged}, c[mid:]...)...)
				case 8: // the checkpoint is not touched, so it still says where the export should end
					c = c[:len(c)-2]
				case 9:
					c = c[1:]
				case 10, 11: // an editor with the whole file: change it and re-hash what follows
					forgeArgs := []string{"--drop", fmt.Sprint(*exportEventOf(t, c[mid]).Seq)}
					if kind != "range" {
						forgeArgs = append(forgeArgs, "--renumber")
					}
					if ti == 11 {
						forgeArgs = []string{"--edit-path", fmt.Sprint(*exportEventOf(t, c[mid]).Seq), "/forged/path"}
					}
					forgedBody := runForge(t, py, bodies[kind], forgeArgs...)
					_, _, forgedRaw := exportLines(t, forgedBody)
					u, c = nil, nil
					fevs, _, _ := exportLines(t, forgedBody)
					for i, ev := range fevs {
						if ev.Seq == nil {
							u = append(u, forgedRaw[i])
						} else {
							c = append(c, forgedRaw[i])
						}
					}
					cpEdit = forgedCheckpoint(t, forgedBody)
				case 12: // the checkpoint still counts the honest number of unchained events
					u = append([]string{forged}, u...)
					cpEdit["unchained"] = len(unch)
				}
				if _, set := cpEdit["unchained"]; !set && ti != 10 && ti != 11 {
					cpEdit["unchained"] = countUnchained(u, c)
				}
				stream := assemble(t, cp, u, c, cpEdit)
				if ti > 0 && stream == bodies[kind] {
					t.Fatalf("%s / %s: the edit changed nothing, so the cell tests nothing", kind, tamper)
				}
				for oi, option := range matrixOptions {
					want := matrixWant[kind][oi][ti]
					got, out := runVerifyExport(t, py, stream, opts[oi]...)
					wantCode := 1
					switch want {
					case mOK:
						wantCode = 0
					case mIN:
						wantCode = 2
					}
					text := want
					if want == mOK {
						text = " 0 breaks"
					}
					if got != wantCode || !strings.Contains(out, text) {
						t.Errorf("%s export, %s, options %s: exit %d, want %d and %q\n%s",
							kind, tamper, option, got, wantCode, text, out)
					}
				}
			}
		}

		// An anchor with the right sequence and the WRONG hash, on an otherwise honest export. For a
		// resumed export this is the only thing that decides the join: every other rule passes.
		bad := strings.Repeat("ab", 32)
		wrongAnchor := map[string][3]string{
			//         wrong head  wrong floor  wrong after
			"full":    {mAM, mAM, mDG},
			"resumed": {mAM, mDG, mJM},
			"range":   {mDG, mDG, mDG},
		}
		for _, kind := range []string{"full", "resumed", "range"} {
			for i, args := range [][]string{
				{"--expect-head", fmt.Sprintf("%d:%s", fullCP.HeadSeq, bad)},
				{"--expect-floor", fmt.Sprintf("%d:%s", fullCP.FloorSeq, bad)},
				{"--expect-after", "6:" + bad},
			} {
				want := wrongAnchor[kind][i]
				if code, out := runVerifyExport(t, py, bodies[kind], args...); code != 1 || !strings.Contains(out, want) {
					t.Errorf("%s export, honest, wrong anchor %v: exit %d, want 1 and %q\n%s", kind, args, code, want, out)
				}
			}
		}

		// What a bare run tells the reader it did NOT establish.
		if _, out := runVerifyExport(t, py, resumed); !strings.Contains(out, "NOTE: coverage not checked") {
			t.Errorf("a resumed export with no options must say coverage was not checked:\n%s", out)
		}
		if _, out := runVerifyExport(t, py, ranged); !strings.Contains(out, "NOTE: coverage not checked") {
			t.Errorf("a range with no options must say coverage was not checked:\n%s", out)
		}
		if _, out := runVerifyExport(t, py, full); !strings.Contains(out, "the checkpoint is unsigned") {
			t.Errorf("a whole export with no options must say its ends are the checkpoint's own:\n%s", out)
		}
		if _, out := runVerifyExport(t, py, full, "--expect-floor", floor); !strings.Contains(out, "no anchor verified any record") {
			t.Errorf("--expect-floor alone must say no record was verified:\n%s", out)
		}
		if _, out := runVerifyExport(t, py, full, "--expect-head", head); !strings.Contains(out, "the start is not anchored") {
			t.Errorf("--expect-head alone must say the start is not anchored:\n%s", out)
		}
		if _, out := runVerifyExport(t, py, full, "--require-full"); !strings.Contains(out, "1 unchained event(s)") {
			t.Errorf("an unchained event must be noted whenever there is one:\n%s", out)
		}
		// The command line: --help prints the docstring; a contradictory or unknown option is
		// exit 2, and is not a finding about the file.
		if code, out := runVerifyExport(t, py, "", "--help"); code != 0 || !strings.Contains(out, "WHAT NO OPTION CAN DO") {
			t.Errorf("--help: exit %d\n%s", code, out)
		}
		for _, args := range [][]string{
			{"--expect-after", after, "--require-full"},
			{"--expect-after", after, "--expect-floor", floor},
			{"--expect-head"},
			{"--expect-head", "nonsense"},
			{"--expect-unchained", "many"},
			{"--nope"},
		} {
			if code, out := runVerifyExport(t, py, full, args...); code != 2 || !strings.Contains(out, "UNREADABLE") {
				t.Errorf("%v: exit %d, want 2 and UNREADABLE\n%s", args, code, out)
			}
		}
	})
}

func exportEventOf(t *testing.T, line string) exportEvent {
	t.Helper()
	var ev exportEvent
	if err := json.Unmarshal([]byte(line), &ev); err != nil || ev.Seq == nil || ev.Hash == nil {
		t.Fatalf("not a chained event line: %v\n%s", err, line)
	}
	return ev
}

// assemble puts a stream together: unchained lines, chained lines, then the checkpoint with its
// count corrected and cpEdit applied.
func assemble(t *testing.T, cp *exportCheckpoint, unch, ch []string, cpEdit map[string]any) string {
	t.Helper()
	raw, _ := json.Marshal(cp)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for k, v := range cpEdit {
		m[k] = v
	}
	m["events"] = len(unch) + len(ch)
	out, _ := json.Marshal(m)
	lines := append(append(append([]string{}, unch...), ch...), string(out))
	return strings.Join(lines, "\n") + "\n"
}

// runVerifyExport runs the reference verifier over stream and returns its exit code and output.
func runVerifyExport(t *testing.T, py, stream string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(py, append([]string{"testdata/audit_chain_reference.py", "verify-export"}, args...)...)
	cmd.Stdin = strings.NewReader(stream)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	code := 0
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("running the reference verifier: %v", err)
		}
		code = ee.ExitCode()
	}
	return code, out.String()
}

// countUnchained counts the lines with no seq, wherever the tamper put them.
func countUnchained(u, c []string) int {
	n := 0
	for _, l := range append(append([]string{}, u...), c...) {
		if strings.Contains(l, `"seq":null`) {
			n++
		}
	}
	return n
}

// runForge runs audit_chain_forge.py over an export.
func runForge(t *testing.T, py, stream string, args ...string) string {
	t.Helper()
	cmd := exec.Command(py, append([]string{"testdata/audit_chain_forge.py"}, args...)...)
	cmd.Stdin = strings.NewReader(stream)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("forging: %v\n%s", err, errb.String())
	}
	return out.String()
}

// forgedCheckpoint returns the forged export's checkpoint as the edits assemble should apply.
func forgedCheckpoint(t *testing.T, body string) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(body), "\n")
	var m map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "type")
	return m
}

// The documented workflow is to record the head and floor on a schedule and check a LATER export
// against them. The matrix takes its anchors from the export it checks, so it could not see that an
// anchor which must equal the checkpoint's head refuses every honest export of a live tenant
// (cleat-review on #2191). This runs the lifecycle for real: anchors, growth, then a retention sweep.
func TestAnAnchorRecordedEarlierStillVerifiesAnHonestLaterExport(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("python3 is not installed, so the reference verifier cannot run: %v", err)
	}
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		p := e.plugin()
		tenant := uuid.New()
		e.record(p, tenant, 8)
		export := func() (string, *exportCheckpoint, map[int64]string) {
			t.Helper()
			code, body := e.get(p, tenant, "/audit/export")
			if code != 200 {
				t.Fatalf("export: %d %s", code, body)
			}
			evs, cp, _ := exportLines(t, body)
			hashes := map[int64]string{}
			for _, ev := range evs {
				if ev.Seq != nil {
					hashes[*ev.Seq] = *ev.Hash
				}
			}
			return body, cp, hashes
		}
		_, cp0, h0 := export()
		head0 := fmt.Sprintf("%d:%s", cp0.HeadSeq, cp0.HeadHash)
		floor0 := fmt.Sprintf("%d:%s", cp0.FloorSeq, cp0.FloorHash)
		mid0 := fmt.Sprintf("4:%s", h0[4])

		// The chain grows by 7 rows: the anchors are now 7 rows behind.
		e.record(p, tenant, 7)
		grown, _, hg := export()
		for name, tc := range map[string]struct {
			args []string
			code int
		}{
			"the head recorded 7 rows ago":  {[]string{"--expect-head", head0}, 0},
			"a mid-chain anchor":            {[]string{"--expect-head", mid0}, 0},
			"the head and the floor":        {[]string{"--expect-head", head0, "--expect-floor", floor0}, 0},
			"the head, floor and unchained": {[]string{"--expect-head", head0, "--expect-floor", floor0, "--expect-unchained", "0"}, 0},
			// The floor anchor alone compares the checkpoint's own floor hash and no record, and
			// the checkpoint is whatever the editor of the file says: it verifies nothing.
			"a floor recorded before then, alone": {[]string{"--expect-floor", floor0}, 2},
		} {
			code, out := runVerifyExport(t, py, grown, tc.args...)
			if code != tc.code {
				t.Errorf("%s vs an honest export after growth: exit %d, want %d\n%s", name, code, tc.code, out)
			}
			if tc.code == 2 && !strings.Contains(out, "INCONCLUSIVE") {
				t.Errorf("%s: exit 2 must say INCONCLUSIVE:\n%s", name, out)
			}
			if strings.Contains(tc.args[0], "head") && !strings.Contains(out, "record(s) above seq") {
				t.Errorf("%s: the 7 newer records are bound only by the checkpoint, and the run must say so:\n%s", name, out)
			}
		}
		// A record at or below the anchor changed, and everything after it re-hashed: the anchor disagrees.
		forged := runForge(t, py, grown, "--edit-path", "3", "/rewritten")
		if code, out := runVerifyExport(t, py, forged, "--expect-head", mid0); code != 1 || !strings.Contains(out, "ANCHOR MISMATCH") {
			t.Errorf("a record below the anchor rewritten and re-hashed: exit %d\n%s", code, out)
		}
		// ...and one ABOVE it is the documented limit: only the checkpoint binds it.
		forgedAbove := runForge(t, py, grown, "--edit-path", "6", "/rewritten")
		if code, out := runVerifyExport(t, py, forgedAbove, "--expect-head", mid0); code != 0 || !strings.Contains(out, "record(s) above seq 4") {
			t.Errorf("a record above the anchor rewritten and re-hashed: exit %d\n%s", code, out)
		}

		// Retention removes rows 1..6, and the floor moves past the old anchors.
		cutoff := e.tsOf(tenant, 6).Add(time.Microsecond)
		if n, err := e.plugin().retainTenant(context.Background(), tenant, cutoff); err != nil || n != 6 {
			t.Fatalf("the sweep removed %d rows, %v", n, err)
		}
		swept, cps, _ := export()
		if cps.FloorSeq != 6 {
			t.Fatalf("the floor is seq %d after the sweep, want 6", cps.FloorSeq)
		}
		// Both anchors are below the moved floor: nothing left to compare either with. That is
		// INCONCLUSIVE, not a pass: a cut file would look exactly like this.
		// The database says the same floor and head as the file's checkpoint: that comparison, at or after
		// the export, is how a file cut to look like a sweep is told from a real one.
		rep, err := VerifyChain(context.Background(), p.db, e.d.dialect, tenant, VerifyOptions{})
		if err != nil || rep.FloorSeq != cps.FloorSeq || rep.FloorHash != cps.FloorHash || rep.HeadSeq != cps.HeadSeq || rep.HeadHash != cps.HeadHash {
			t.Errorf("the verify report says floor %d %q and head %d %q, the export's checkpoint floor %d %q head %d %q (%v)",
				rep.FloorSeq, rep.FloorHash, rep.HeadSeq, rep.HeadHash, cps.FloorSeq, cps.FloorHash, cps.HeadSeq, cps.HeadHash, err)
		}
		code, out := runVerifyExport(t, py, swept, "--expect-floor", floor0, "--expect-head", mid0)
		if code != 2 || strings.Count(out, "verified NOTHING") != 2 || !strings.Contains(out, "INCONCLUSIVE") {
			t.Errorf("every anchor retired must be INCONCLUSIVE (exit 2): exit %d\n%s", code, out)
		}
		// One anchor still inside the export verifies it, and a retired floor beside it is only a NOTE:
		// that is every honest sweep.
		code, out = runVerifyExport(t, py, swept, "--expect-floor", floor0, "--expect-head", head0)
		if code != 0 || strings.Count(out, "verified NOTHING") != 1 {
			t.Errorf("a retired floor beside a verified head must be exit 0 with one NOTE: exit %d\n%s", code, out)
		}
		// The seq the floor now sits at is compared with the checkpoint's floor hash, and that
		// alone verifies nothing (a wrong hash there is still a finding: a finding beats INCONCLUSIVE).
		if code, out := runVerifyExport(t, py, swept, "--expect-floor", fmt.Sprintf("6:%s", hg[6])); code != 2 || !strings.Contains(out, "INCONCLUSIVE") {
			t.Errorf("an anchor at the new floor, alone: exit %d\n%s", code, out)
		}
		if code, out := runVerifyExport(t, py, swept, "--expect-floor", "6:"+strings.Repeat("ab", 32)); code != 1 || !strings.Contains(out, "ANCHOR MISMATCH") {
			t.Errorf("a wrong hash at the new floor: exit %d\n%s", code, out)
		}
		// Rolled back: the export is older than an anchor recorded after it.
		if code, out := runVerifyExport(t, py, swept, "--expect-head", fmt.Sprintf("%d:%s", cps.HeadSeq+5, strings.Repeat("cd", 32))); code != 1 || !strings.Contains(out, "cut back") {
			t.Errorf("an anchor above the export's head: exit %d\n%s", code, out)
		}
	})
}

// Retirement and "the export starts here" are decided by the checkpoint, which whoever edits the
// file also writes. So an anchor that matches no RECORD verifies nothing, and a file edited to cut
// below every anchor must not read as verified (cleat-review on #2191, five measured forgeries).
func TestAFileCutBelowEveryAnchorIsInconclusiveNotVerified(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("python3 is not installed, so the reference verifier cannot run: %v", err)
	}
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		p := e.plugin()
		tenant := uuid.New()
		e.record(p, tenant, 15)
		_, body := e.get(p, tenant, "/audit/export")
		evs, cp, _ := exportLines(t, body)
		hashOf := func(seq int64) string {
			for _, ev := range evs {
				if ev.Seq != nil && *ev.Seq == seq {
					return *ev.Hash
				}
			}
			t.Fatalf("no seq %d", seq)
			return ""
		}
		// The anchors cleat-review used: the head at seq 10 and the genesis floor.
		anchors := []string{"--expect-head", fmt.Sprintf("10:%s", hashOf(10)), "--expect-floor", fmt.Sprintf("0:%s", cp.FloorHash)}
		if code, out := runVerifyExport(t, py, body, anchors...); code != 0 {
			t.Fatalf("the honest export with both anchors: exit %d\n%s", code, out)
		}
		for name, forge := range map[string][]string{
			"a cut of 1..10 with the floor moved to 10":         {"--cut-below", "10"},
			"a cut of 1..12 with the floor moved to 12":         {"--cut-below", "12"},
			"a cut past every anchor":                           {"--cut-below", "11"},
			"one wholly forged record and a forged floor":       {"--cut-below", "14", "--floor-hash", strings.Repeat("ee", 32)},
			"a cut of 1..10 and 11..15 rewritten and re-hashed": {"--cut-below", "10", "--edit-path", "12", "/forged"},
		} {
			forged := runForge(t, py, body, forge...)
			code, out := runVerifyExport(t, py, forged, anchors...)
			if code != 2 || !strings.Contains(out, "INCONCLUSIVE") {
				t.Errorf("%s: exit %d, want 2 INCONCLUSIVE (a green here is a forgery that verified)\n%s", name, code, out)
			}
		}
		// The documented residual: a cut BELOW the highest verified anchor is byte-identical, offline, to
		// an honest sweep, and verifies. The database's floor (verify --json) is what tells them apart.
		cutBelowAnchor := runForge(t, py, body, "--cut-below", "5")
		if code, out := runVerifyExport(t, py, cutBelowAnchor, anchors...); code != 0 || !strings.Contains(out, "verified NOTHING") {
			t.Errorf("a cut below the verified head anchor: exit %d, want 0 with the retired floor noted\n%s", code, out)
		}
	})
}

// Retention removes old unchained rows, so a count recorded earlier is a ceiling and not an
// equality: an honest later export after a routine sweep has FEWER (cleat-review on #2191).
func TestAnUnchainedCountRecordedEarlierIsACeilingAcrossASweep(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("python3 is not installed, so the reference verifier cannot run: %v", err)
	}
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		p := e.plugin()
		tenant := uuid.New()
		e.record(p, tenant, 6)
		e.legacyRow(tenant, 200)
		e.legacyRow(tenant, 100)
		_, before := e.get(p, tenant, "/audit/export")
		_, cp0, _ := exportLines(t, before)
		if cp0.Unchained != 2 {
			t.Fatalf("the checkpoint counts %d unchained, want 2", cp0.Unchained)
		}
		head := fmt.Sprintf("%d:%s", cp0.HeadSeq, cp0.HeadHash)
		// A sweep removes the older legacy row only.
		cutoff := time.Now().Add(-150 * 24 * time.Hour).UnixMicro()
		if n, err := e.plugin().retainUnchained(plugin.ForTenant(context.Background(), tenant), tenant, cutoff); err != nil || n != 1 {
			t.Fatalf("the sweep removed %d unchained rows, %v; want 1", n, err)
		}
		_, after := e.get(p, tenant, "/audit/export")
		if code, out := runVerifyExport(t, py, after, "--expect-head", head, "--expect-unchained", "2"); code != 0 || !strings.Contains(out, "cannot be told apart from a retention sweep") {
			t.Errorf("an honest export after a sweep against the count recorded before it: exit %d\n%s", code, out)
		}
		// Rows added after the chain exists never happen in production; the ceiling is what catches them.
		e.legacyRow(tenant, 50)
		_, more := e.get(p, tenant, "/audit/export")
		if code, out := runVerifyExport(t, py, more, "--expect-head", head, "--expect-unchained", "2"); code != 0 {
			t.Fatalf("2 present against at most 2: exit %d\n%s", code, out) // 1 (kept) + 1 (new) = 2: still within
		}
		e.legacyRow(tenant, 40)
		_, tooMany := e.get(p, tenant, "/audit/export")
		if code, out := runVerifyExport(t, py, tooMany, "--expect-head", head, "--expect-unchained", "2"); code != 1 || !strings.Contains(out, "UNCHAINED COUNT") {
			t.Errorf("3 present against at most 2: exit %d\n%s", code, out)
		}
	})
}
