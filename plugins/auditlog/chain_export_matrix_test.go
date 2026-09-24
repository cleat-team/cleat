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
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
	"testing"
	"time"

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
	mNA  = "n/a"
)

// matrixOptions are the columns' option sets, in the order of the rows below.
var matrixOptions = []string{
	"(none)", "--require-full", "--expect-head", "--expect-floor", "--expect-after",
	"head + floor", "head + after", "--expect-unchained", "best for the kind",
}

// matrixTampers are the rows' edits. Each is made SELF-CONSISTENT the way a forger would: the
// checkpoint's count is adjusted, and where a deletion moves an end the checkpoint's end moves
// with it. Nothing is re-hashed: a rewrite of the hashes is caught by any anchor, and is the
// documented limit without one.
var matrixTampers = []string{
	"none", "delete first record", "delete a middle record", "delete last two records",
	"delete middle + relabel as range", "unchained added after the chain",
	"unchained added at the front", "unchained added mid-stream",
	"delete last two, checkpoint left alone", "delete first record, checkpoint left alone",
}

// matrixWant[kind][option][tamper]. Read a row as "with these options, what does each tamper do".
var matrixWant = map[string][9][10]string{
	"full": {
		//               none  del-first del-mid del-last relabel fake-tail fake-front fake-mid del-last* del-first*
		/* (none)     */ {mOK, mOK, mGAP, mOK, mOK, mUA, mOK, mUA, mME, mMS},
		/* req-full   */ {mOK, mOK, mGAP, mOK, mDG, mUA, mOK, mUA, mME, mMS},
		/* head       */ {mOK, mOK, mGAP, mAM, mDG, mUA, mOK, mUA, mME, mMS},
		/* floor      */ {mOK, mAM, mGAP, mOK, mDG, mUA, mOK, mUA, mME, mMS},
		/* after      */ {mDG, mDG, mGAP, mDG, mDG, mUA, mDG, mUA, mME, mMS},
		/* head+floor */ {mOK, mAM, mGAP, mAM, mDG, mUA, mOK, mUA, mME, mMS},
		/* head+after */ {mDG, mDG, mGAP, mDG, mDG, mUA, mDG, mUA, mME, mMS},
		/* unchained  */ {mOK, mOK, mGAP, mOK, mDG, mUA, mUC, mUA, mME, mMS},
		/* best       */ {mOK, mAM, mGAP, mAM, mDG, mUA, mUC, mUA, mME, mMS},
	},
	"resumed": {
		/* (none)     */ {mOK, mOK, mGAP, mOK, mOK, mUA, mUR, mUA, mME, mMS},
		/* req-full   */ {mDG, mDG, mGAP, mDG, mDG, mUA, mUR, mUA, mME, mMS},
		/* head       */ {mDG, mDG, mGAP, mDG, mDG, mUA, mUR, mUA, mME, mMS},
		/* floor      */ {mDG, mDG, mGAP, mDG, mDG, mUA, mUR, mUA, mME, mMS},
		/* after      */ {mOK, mAM, mGAP, mOK, mDG, mUA, mUR, mUA, mME, mMS},
		/* head+floor */ {mDG, mDG, mGAP, mDG, mDG, mUA, mUR, mUA, mME, mMS},
		/* head+after */ {mOK, mAM, mGAP, mAM, mDG, mUA, mUR, mUA, mME, mMS},
		/* unchained  */ {mDG, mDG, mGAP, mDG, mDG, mUA, mUR, mUA, mME, mMS},
		/* best       */ {mOK, mAM, mGAP, mAM, mDG, mUA, mUR, mUA, mME, mMS},
	},
	"range": {
		/* (none)     */ {mOK, mOK, mOK, mOK, mNA, mUA, mOK, mUA, mOK, mOK},
		/* req-full   */ {mDG, mDG, mDG, mDG, mNA, mUA, mDG, mUA, mDG, mDG},
		/* head       */ {mDG, mDG, mDG, mDG, mNA, mUA, mDG, mUA, mDG, mDG},
		/* floor      */ {mDG, mDG, mDG, mDG, mNA, mUA, mDG, mUA, mDG, mDG},
		/* after      */ {mDG, mDG, mDG, mDG, mNA, mUA, mDG, mUA, mDG, mDG},
		/* head+floor */ {mDG, mDG, mDG, mDG, mNA, mUA, mDG, mUA, mDG, mDG},
		/* head+after */ {mDG, mDG, mDG, mDG, mNA, mUA, mDG, mUA, mDG, mDG},
		/* unchained  */ {mDG, mDG, mDG, mDG, mNA, mUA, mDG, mUA, mDG, mDG},
		/* best       */ {mDG, mDG, mDG, mDG, mNA, mUA, mDG, mUA, mDG, mDG},
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
			}
			switch kind {
			case "resumed":
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
				}
				stream := assemble(t, cp, u, c, cpEdit)
				if ti > 0 && stream == bodies[kind] {
					t.Fatalf("%s / %s: the edit changed nothing, so the cell tests nothing", kind, tamper)
				}
				for oi, option := range matrixOptions {
					want := matrixWant[kind][oi][ti]
					got, out := runVerifyExport(t, py, stream, opts[oi]...)
					wantCode := 1
					if want == mOK {
						wantCode = 0
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
		if _, out := runVerifyExport(t, py, full, "--expect-floor", floor); !strings.Contains(out, "the end is not anchored") {
			t.Errorf("--expect-floor alone must say the end is not anchored:\n%s", out)
		}
		if _, out := runVerifyExport(t, py, full, "--expect-head", head); !strings.Contains(out, "the start is not anchored") {
			t.Errorf("--expect-head alone must say the start is not anchored:\n%s", out)
		}
		if _, out := runVerifyExport(t, py, full, "--require-full"); !strings.Contains(out, "1 unchained event(s) are not covered") {
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
