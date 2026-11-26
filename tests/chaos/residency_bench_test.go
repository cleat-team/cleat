//go:build chaos && bench

package chaos

// A repeatable latency benchmark for the resident-run path (cleat#4187, the BENCHMARK half; the tuning half is not here and no residency constant is changed by anything in this file).
//
// Real cleat-worker binary, real PostgreSQL, the recording guest service of this harness, one worker at a time, --concurrency 1 and the production --poll of 500 ms. Each worker runs with
// residency ON at the product defaults ("on"), with residency OFF (--resident-max-runs 0, "off"), and ON a second time ("on2"): on2 is the same configuration as "on" run at
// another point in the same run. Its gap to "on" is printed as a DESCRIPTION of how far two runs of one thing differ here, and is not a threshold: under no effect off and on2 are exchangeable.
// What is claimed is the separation of the per-rep medians (do all R off reps lie on one side of all 2R on/on2 reps? p = 2/C(3R,R) under no effect, 1/42 at R = 3).
//
// Rows (each one a table below, one line per arm):
//
//	start     the API start's request to the guest's first call arriving at the service.
//	signal    the signal's HTTP commit to the guest's next call, after the run has parked: held (a wait inside the 10 s window, ON) and suspended (a wait past it, ON; every wait, OFF).
//	timer     a sleep of D: the next call's arrival minus the previous one's, minus D.
//	history   the signal row again, with K calls in the history before the wait: what replay costs as a run grows, and what holding it saves.
//	queries   statements the worker's role executes per wait cycle, from pg_stat_statements. Without the extension the row is UNMEASURED, and says so; it is never a zero.
//
// How noise is kept visible: trial 0 of every cell is discarded (it compiles the module); the arms are rotated across reps, never run all-ON then all-OFF, so drift lands on every arm;
// the per-rep medians and their range are printed beside the pooled percentiles; the load average is read before and after every cell and a cell above the core count is marked NOISY; a
// trial whose run did not take the path the cell is about (held when it should be suspended, or the reverse; read per trial in EVERY row, never assumed) is EXCLUDED and counted, never averaged in; a cell that cannot establish its
// precondition is UNMEASURED with the reason. A p99 over fewer than 100 samples is a maximum, and the line says so.
//
// Run:
//
//	CLEAT_CRASH_DB='postgres://…' go test -tags 'chaos bench' ./tests/chaos -run TestBench -count=1 -timeout 180m -v
//
// Knobs (environment): CLEAT_BENCH_TRIALS (default 30, per fast cell, after the discarded first), CLEAT_BENCH_REPS (default 3), CLEAT_BENCH_ROWS (default "start,signal,timer,history,queries"),
// CLEAT_BENCH_K (default "0,10,100,500"), CLEAT_BENCH_OUT (a file; one JSON object per cell and rep, plus a header with the configuration), CLEAT_BENCH_NOISY_LOAD (default: the core count).
// CLEAT_CHAOS_GUEST=python runs the same rows with the Python guest, as the rest of this harness does.

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

type benchConfig struct {
	Trials, Reps int
	Rows         map[string]bool
	Ks           []int
	Out          string
	NoisyLoad    float64
	Poll         string
}

func benchEnvInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

func loadBenchConfig() benchConfig {
	c := benchConfig{Trials: benchEnvInt("CLEAT_BENCH_TRIALS", 30), Reps: benchEnvInt("CLEAT_BENCH_REPS", 3), Rows: map[string]bool{}, Out: os.Getenv("CLEAT_BENCH_OUT"), NoisyLoad: float64(runtime.NumCPU()), Poll: "500ms"}
	if v := os.Getenv("CLEAT_BENCH_POLL"); v != "" {
		if _, err := time.ParseDuration(v); err == nil {
			c.Poll = v
		}
	}
	rows := os.Getenv("CLEAT_BENCH_ROWS")
	if rows == "" {
		rows = "start,signal,timer,history,queries"
	}
	for _, r := range strings.Split(rows, ",") {
		c.Rows[strings.TrimSpace(r)] = true
	}
	ks := os.Getenv("CLEAT_BENCH_K")
	if ks == "" {
		ks = "0,10,100,500"
	}
	for _, k := range strings.Split(ks, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(k)); err == nil && n >= 0 {
			c.Ks = append(c.Ks, n)
		}
	}
	if v, err := strconv.ParseFloat(os.Getenv("CLEAT_BENCH_NOISY_LOAD"), 64); err == nil && v > 0 {
		c.NoisyLoad = v
	}
	return c
}

// benchArm is one worker configuration. nil flags leave the product default in force: that is what "on" means.
type benchArm struct {
	name  string
	flags []string
}

var benchArms = []benchArm{{"on", nil}, {"off", residencyOff}, {"on2", nil}}

// benchSample is one cell of one rep of one arm.
type benchSample struct {
	Row, Cell, Arm string
	Rep            int
	Durations      []time.Duration `json:"-"`
	MS             []float64       `json:"samples_ms,omitempty"`
	Excluded       int             `json:"excluded_wrong_path"`
	Paths          map[string]int  `json:"paths,omitempty"`
	LoadBefore     float64         `json:"load_before"`
	LoadAfter      float64         `json:"load_after"`
	Noisy          bool            `json:"noisy"`
	Unmeasured     string          `json:"unmeasured,omitempty"`
	Extra          map[string]any  `json:"extra,omitempty"`
}

var loadRe = regexp.MustCompile(`load averages?: *([0-9]+[.,]?[0-9]*)`)

func benchLoad() float64 {
	out, err := exec.Command("uptime").Output()
	if err != nil {
		return -1
	}
	m := loadRe.FindSubmatch(out)
	if m == nil {
		return -1
	}
	f, _ := strconv.ParseFloat(strings.Replace(string(m[1]), ",", ".", 1), 64)
	return f
}

// benchPercentile is the nearest-rank percentile of an ascending slice: the smallest value with at least p of the samples at or below it.
func benchPercentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(p*float64(len(sorted)) + 0.999999999)
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

func benchSorted(d []time.Duration) []time.Duration {
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s
}

func benchMedian(d []time.Duration) time.Duration {
	s := benchSorted(d)
	if len(s) == 0 {
		return 0
	}
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

// The percentile and median code is the one thing in this file whose answer is known without running a worker, so it has a known positive of its own.
func TestBenchPercentilesAreRight(t *testing.T) {
	var d []time.Duration
	for i := 1; i <= 100; i++ {
		d = append(d, time.Duration(i)*time.Millisecond)
	}
	s := benchSorted(d)
	for _, tc := range []struct {
		p    float64
		want time.Duration
	}{{0.50, 50 * time.Millisecond}, {0.90, 90 * time.Millisecond}, {0.99, 99 * time.Millisecond}, {1.0, 100 * time.Millisecond}} {
		if got := benchPercentile(s, tc.p); got != tc.want {
			t.Errorf("p%v of 1..100 ms = %v, want %v", tc.p*100, got, tc.want)
		}
	}
	// Five samples: p99 is the maximum, which is why the report says so for N under 100.
	five := benchSorted([]time.Duration{5 * time.Millisecond, 1 * time.Millisecond, 4 * time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond})
	if got := benchPercentile(five, 0.99); got != 5*time.Millisecond {
		t.Errorf("p99 of five samples = %v, want the maximum, 5ms", got)
	}
	if got := benchMedian(five); got != 3*time.Millisecond {
		t.Errorf("median of 1..5 ms = %v, want 3ms", got)
	}
	if got := benchMedian([]time.Duration{1 * time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond, 10 * time.Millisecond}); got != 2500*time.Microsecond {
		t.Errorf("median of 1, 2, 3, 10 ms = %v, want 2.5ms", got)
	}
}

// benchCtx is one worker, one arm, one rep.
type benchCtx struct {
	t      *testing.T
	env    *env
	addr   string
	key    string
	queue  string
	arm    benchArm
	rep    int
	cfg    benchConfig
	tag    string
	client *http.Client
}

func benchAPIKey(t *testing.T, bin string) string {
	t.Helper()
	var last string
	for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		out, err := exec.Command(bin, "--driver=postgres", "--db="+appDSN(t), "--generate-api-key="+defaultTenant).CombinedOutput()
		last = string(out)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "Key:") {
				if f := strings.Fields(line); len(f) == 2 {
					return f[1]
				}
			}
		}
	}
	t.Fatalf("UNMEASURED: no API key could be minted:\n%s", last)
	return ""
}

// benchAwaitAPI waits until the worker's API answers /metrics with real cleat series: a start sent before that is refused, and the cell would measure the wait for the listener.
func benchAwaitAPI(t *testing.T, addr string) {
	t.Helper()
	for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if _, ok := residentRuns(addr); ok {
			return
		}
	}
	t.Fatalf("UNMEASURED: the worker's API at %s never came up", addr)
}

func (c *benchCtx) request(method, path, body string) (*http.Response, []byte, error) {
	req, err := http.NewRequest(method, "http://"+c.addr+path, bytes.NewBufferString(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b, nil
}

// startRun starts the script through the worker's API, the way a client does, and returns the run's id and the instant just BEFORE the request was sent.
func (c *benchCtx) start(label string, sc scenario) (id string, t0 time.Time, err error) {
	body, _ := json.Marshal(map[string]any{"input": json.RawMessage(scriptInput(label, sc))})
	t0 = time.Now()
	resp, raw, err := c.request(http.MethodPost, "/api/workflows/chaosscript/start", string(body))
	if err != nil {
		return "", t0, err
	}
	if resp.StatusCode/100 != 2 {
		return "", t0, fmt.Errorf("POST start: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.ID == "" {
		return "", t0, fmt.Errorf("the start reply carried no run id: %s", strings.TrimSpace(string(raw)))
	}
	c.t.Cleanup(func() {
		_, _ = c.env.db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, out.ID)
		_, _ = c.env.db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, out.ID)
	})
	return out.ID, t0, nil
}

func (c *benchCtx) signal(id, name string) error {
	resp, raw, err := c.request(http.MethodPost, "/api/workflows/"+id+"/signal", fmt.Sprintf(`{"signal_name":%q,"payload":"{}"}`, name))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("POST signal: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

// arrival is when the nth successful request of op reached the service, polled at 1 ms.
func (c *benchCtx) arrival(label, op string, nth int, within time.Duration) (time.Time, bool) {
	for deadline := time.Now().Add(within); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		n := 0
		for _, r := range c.env.svc.requestsOf(label) {
			if r.Op == op && !r.Failed {
				n++
				if n == nth {
					return r.At, true
				}
			}
		}
	}
	return time.Time{}, false
}

// parked waits until the run is asleep and says on which path: "held" (the worker holds it in memory) or "suspended" (it is parked in the store and the worker holds nothing).
func (c *benchCtx) parked(id string, within time.Duration) (string, bool) {
	for deadline := time.Now().Add(within); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		n, ok := residentRuns(c.addr)
		if !ok {
			continue
		}
		if n >= 1 {
			return "held", true
		}
		if parkedNow(c.env.db, id) {
			return "suspended", true
		}
	}
	return "", false
}

// pathNow says which path a run that has just reached its wait is on, as soon as it is on one: "held" (the worker has it in memory) or "suspended" (the lease row is ready, unassigned and due
// later). A sleep inside the window is short, so unlike parked this does not wait for a wake time more than 2 s away.
func (c *benchCtx) pathNow(id string, within time.Duration) (string, bool) {
	for deadline := time.Now().Add(within); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if n, ok := residentRuns(c.addr); ok && n >= 1 {
			return "held", true
		}
		var n int
		_ = c.env.db.QueryRow(`SELECT count(*) FROM workflow_leases WHERE id = $1 AND status = 'ready' AND assigned_to IS NULL AND next_wake_at > now()`, id).Scan(&n)
		if n > 0 {
			return "suspended", true
		}
	}
	return "", false
}

func (c *benchCtx) done(id string) bool {
	_, _, _, ok := awaitStatus(c.env.db, id, 60*time.Second, func(s string) bool { return s != "ready" && s != "running" })
	return ok
}

// newSample starts a cell: the load before is read here, the load after in finish.
func (c *benchCtx) newSample(row, cell string) *benchSample {
	return &benchSample{Row: row, Cell: cell, Arm: c.arm.name, Rep: c.rep, Paths: map[string]int{}, LoadBefore: benchLoad()}
}

func (c *benchCtx) finish(s *benchSample) *benchSample {
	s.LoadAfter = benchLoad()
	s.Noisy = s.LoadBefore > c.cfg.NoisyLoad || s.LoadAfter > c.cfg.NoisyLoad
	for _, d := range s.Durations {
		s.MS = append(s.MS, float64(d.Microseconds())/1000)
	}
	return s
}

func (c *benchCtx) label(cell string, trial int) string {
	return fmt.Sprintf("bench-%s-%s-%s-r%d-%d", c.tag, c.arm.name, cell, c.rep, trial)
}

// ---- row: start

func (c *benchCtx) rowStart() *benchSample {
	s := c.newSample("start", "start to first step")
	sc := scenario{"bench_start", []op{{Op: "call", Name: "A"}}, nil}
	for i := 0; i <= c.cfg.Trials; i++ {
		label := c.label("start", i)
		id, t0, err := c.start(label, sc)
		if err != nil {
			s.Unmeasured = "start: " + err.Error()
			break
		}
		at, ok := c.arrival(label, "A", 1, 60*time.Second)
		if !ok {
			s.Unmeasured = fmt.Sprintf("trial %d: the guest's first step never arrived", i)
			break
		}
		if !c.done(id) {
			s.Unmeasured = fmt.Sprintf("trial %d: the run did not finish", i)
			break
		}
		if i > 0 {
			s.Durations = append(s.Durations, at.Sub(t0))
		}
	}
	return c.finish(s)
}

// ---- row: signal (and history, which is the same row with K calls before the wait)

// waitCell is the signal row at one wait length with K calls in the history. wantPath(arm) says which path the cell is about for the arm: a trial that took another is excluded.
func (c *benchCtx) rowSignal(row, cell string, k int, waitMs int64, wantPath string, trials int) *benchSample {
	s := c.newSample(row, cell)
	ops := []op{{Op: "call", Name: "A"}}
	for i := 0; i < k; i++ {
		ops = append(ops, op{Op: "call", Name: "H"})
	}
	ops = append(ops, op{Op: "call", Name: "Z"}, op{Op: "await_signal", Name: "go", Ms: waitMs, Hit: "HIT", Miss: "MISS"}, op{Op: "call", Name: "B"})
	sc := scenario{"bench_signal", ops, nil}
	for i := 0; i <= trials; i++ {
		label := c.label(cell, i)
		id, _, err := c.start(label, sc)
		if err != nil {
			s.Unmeasured = "start: " + err.Error()
			break
		}
		if _, ok := c.arrival(label, "Z", 1, 120*time.Second); !ok {
			s.Unmeasured = fmt.Sprintf("trial %d: the guest never reached the wait", i)
			break
		}
		path, ok := c.parked(id, 60*time.Second)
		if !ok {
			s.Unmeasured = fmt.Sprintf("trial %d: the run never parked", i)
			break
		}
		time.Sleep(150 * time.Millisecond) // the worker has just finished the step before the wait: the window the poll tick used to cover
		t0 := time.Now()
		if err := c.signal(id, "go"); err != nil {
			s.Unmeasured = fmt.Sprintf("trial %d: signal: %v", i, err)
			break
		}
		at, ok := c.arrival(label, "HIT", 1, 60*time.Second)
		if !ok {
			s.Unmeasured = fmt.Sprintf("trial %d: the guest's next step (HIT) never arrived", i)
			break
		}
		if !c.done(id) {
			s.Unmeasured = fmt.Sprintf("trial %d: the run did not finish", i)
			break
		}
		if i == 0 {
			continue
		}
		s.Paths[path]++
		if wantPath != "" && path != wantPath {
			s.Excluded++
			continue
		}
		s.Durations = append(s.Durations, at.Sub(t0))
	}
	return c.finish(s)
}

// ---- row: timer

func (c *benchCtx) rowTimer(cell string, d time.Duration, wantPath string, trials int) *benchSample {
	s := c.newSample("timer", cell)
	sc := scenario{"bench_timer", []op{{Op: "call", Name: "A"}, {Op: "sleep", Ms: d.Milliseconds()}, {Op: "call", Name: "B"}}, nil}
	for i := 0; i <= trials; i++ {
		label := c.label(cell, i)
		id, _, err := c.start(label, sc)
		if err != nil {
			s.Unmeasured = "start: " + err.Error()
			break
		}
		a, ok := c.arrival(label, "A", 1, 120*time.Second)
		if !ok {
			s.Unmeasured = fmt.Sprintf("trial %d: the first step never arrived", i)
			break
		}
		path, ok := c.pathNow(id, 5*time.Second)
		if !ok {
			s.Unmeasured = fmt.Sprintf("trial %d: the run was neither held nor suspended during its sleep", i)
			break
		}
		b, ok := c.arrival(label, "B", 1, d+60*time.Second)
		if !ok {
			s.Unmeasured = fmt.Sprintf("trial %d: the step after the sleep never arrived", i)
			break
		}
		if !c.done(id) {
			s.Unmeasured = fmt.Sprintf("trial %d: the run did not finish", i)
			break
		}
		if i == 0 {
			continue
		}
		s.Paths[path]++
		if wantPath != "" && path != wantPath {
			s.Excluded++
			continue
		}
		s.Durations = append(s.Durations, b.Sub(a)-d)
	}
	return c.finish(s)
}

// ---- row: queries (needs pg_stat_statements)

func benchPGSS(db *sql.DB) string {
	if _, err := db.Exec(`CREATE EXTENSION IF NOT EXISTS pg_stat_statements`); err != nil {
		return "CREATE EXTENSION pg_stat_statements: " + err.Error()
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM pg_stat_statements`).Scan(&n); err != nil {
		return "reading pg_stat_statements (is it in shared_preload_libraries?): " + err.Error()
	}
	return ""
}

func (c *benchCtx) pgssCalls(role string) float64 {
	var n sql.NullFloat64
	_ = c.env.db.QueryRow(`SELECT sum(s.calls) FROM pg_stat_statements s JOIN pg_roles r ON r.oid = s.userid WHERE r.rolname = $1`, role).Scan(&n)
	return n.Float64
}

func (c *benchCtx) rowQueries(waits int) *benchSample {
	s := c.newSample("queries", "statements per wait (8 s wait, signalled)")
	if why := benchPGSS(c.env.db); why != "" {
		s.Unmeasured = "pg_stat_statements not available: " + why
		return c.finish(s)
	}
	u, err := url.Parse(appDSN(c.t))
	if err != nil || u.User == nil {
		s.Unmeasured = "cannot tell the worker's database role from the DSN"
		return c.finish(s)
	}
	role := u.User.Username()
	// The idle rate first: the heartbeat, the poll and the reaper execute statements whether or not a wait is in flight, so a per-wait total without it is not a per-wait cost.
	_, _ = c.env.db.Exec(`SELECT pg_stat_statements_reset()`)
	idleFor := 10 * time.Second
	time.Sleep(idleFor)
	idle := c.pgssCalls(role) / idleFor.Seconds()

	sc := scenario{"bench_queries", []op{{Op: "call", Name: "A"}, {Op: "call", Name: "Z"}, {Op: "await_signal", Name: "go", Ms: 8000, Hit: "HIT", Miss: "MISS"}, {Op: "call", Name: "B"}}, nil}
	// One discarded cycle first (it compiles the module), then the measured ones in one stretch.
	cycle := func(label string) bool {
		id, _, err := c.start(label, sc)
		if err != nil {
			s.Unmeasured = "start: " + err.Error()
			return false
		}
		if _, ok := c.arrival(label, "Z", 1, 120*time.Second); !ok {
			s.Unmeasured = "the guest never reached the wait"
			return false
		}
		if _, ok := c.parked(id, 60*time.Second); !ok {
			s.Unmeasured = "the run never parked"
			return false
		}
		time.Sleep(150 * time.Millisecond)
		if err := c.signal(id, "go"); err != nil {
			s.Unmeasured = "signal: " + err.Error()
			return false
		}
		if _, ok := c.arrival(label, "HIT", 1, 60*time.Second); !ok {
			s.Unmeasured = "the guest's next step never arrived"
			return false
		}
		return c.done(id)
	}
	if !cycle(c.label("queries", 0)) {
		return c.finish(s)
	}
	_, _ = c.env.db.Exec(`SELECT pg_stat_statements_reset()`)
	t0 := time.Now()
	for i := 1; i <= waits; i++ {
		if !cycle(c.label("queries", i)) {
			return c.finish(s)
		}
	}
	wall := time.Since(t0)
	total := c.pgssCalls(role)
	perWait := total / float64(waits)
	net := (total - idle*wall.Seconds()) / float64(waits)
	s.Extra = map[string]any{"waits": waits, "statements_total": total, "statements_per_wait_raw": perWait, "idle_statements_per_second": idle, "wall_seconds": wall.Seconds(), "statements_per_wait_minus_idle": net}
	return c.finish(s)
}

// ---- the run

func benchHardware() map[string]string {
	h := map[string]string{"go": runtime.Version(), "goos": runtime.GOOS, "goarch": runtime.GOARCH, "ncpu": strconv.Itoa(runtime.NumCPU())}
	for k, args := range map[string][]string{"cpu": {"sysctl", "-n", "machdep.cpu.brand_string"}, "mem_bytes": {"sysctl", "-n", "hw.memsize"}, "os": {"sw_vers", "-productVersion"}} {
		if out, err := exec.Command(args[0], args[1:]...).Output(); err == nil {
			h[k] = strings.TrimSpace(string(out))
		}
	}
	return h
}

func TestBenchResidencyLatencyOnPostgres(t *testing.T) {
	cfg := loadBenchConfig()
	env := newEnv(t)
	tag := fmt.Sprintf("%d", time.Now().Unix())
	gitSHA := ""
	if out, err := exec.Command("git", "-C", tree(t), "rev-parse", "HEAD").Output(); err == nil {
		gitSHA = strings.TrimSpace(string(out))
	}
	header := map[string]any{"kind": "config", "tree_sha": gitSHA, "trials": cfg.Trials, "reps": cfg.Reps, "ks": cfg.Ks, "guest": guestLanguage(), "dialect": "postgres", "poll": cfg.Poll, "concurrency": 1,
		"noisy_load_threshold": cfg.NoisyLoad, "hardware": benchHardware(), "load_at_start": benchLoad(), "chaos_resident_max_runs_env": os.Getenv("CLEAT_CHAOS_RESIDENT_MAX_RUNS")}
	t.Logf("BENCH config: %v", header)

	var results []*benchSample
	for rep := 0; rep < cfg.Reps; rep++ {
		// Rotate the arm order each rep so drift in load lands on every arm.
		for i := range benchArms {
			arm := benchArms[(i+rep)%len(benchArms)]
			queue := fmt.Sprintf("bench-%s-%s-r%d", tag, arm.name, rep)
			addr := freeLoopbackAddr(t)
			deploy(t, env.db, env.wasm, queue)
			w := startWorker(t, env.bin, queue, env.svc.srv.URL, append([]string{"--api-addr", addr, "--poll", cfg.Poll}, arm.flags...)...)
			benchAwaitAPI(t, addr)
			c := &benchCtx{t: t, env: env, addr: addr, key: benchAPIKey(t, env.bin), queue: queue, arm: arm, rep: rep, cfg: cfg, tag: tag, client: &http.Client{Timeout: 30 * time.Second}}
			fast := cfg.Trials
			if cfg.Rows["start"] {
				results = append(results, c.rowStart())
			}
			// want says which path a cell is about for this arm: residency off suspends every wait, residency on holds one inside the window and suspends one past it.
			want := func(inWindow bool) string {
				if arm.name == "off" || !inWindow {
					return "suspended"
				}
				return "held"
			}
			if cfg.Rows["signal"] {
				results = append(results, c.rowSignal("signal", "signal to resume, 8 s wait (inside the 10 s window)", 0, 8000, want(true), fast))
				results = append(results, c.rowSignal("signal", "signal to resume, 20 s wait (past the window)", 0, 20000, want(false), fast))
			}
			if cfg.Rows["timer"] {
				results = append(results, c.rowTimer("timer lateness, sleep 2 s (inside the window)", 2*time.Second, want(true), min(fast, 15)))
				results = append(results, c.rowTimer("timer lateness, sleep 12 s (past the window)", 12*time.Second, want(false), min(fast, 5)))
			}
			if cfg.Rows["history"] {
				for _, k := range cfg.Ks {
					results = append(results, c.rowSignal("history", fmt.Sprintf("signal to resume, 8 s wait, K=%d prior calls", k), k, 8000, want(true), min(fast, 10)))
				}
			}
			if cfg.Rows["queries"] {
				results = append(results, c.rowQueries(min(fast, 10)))
			}
			w.kill()
		}
	}
	if n := reportBench(t, cfg, header, results); n > 0 {
		t.Errorf("UNMEASURED: %d cell(s) could not establish their precondition (listed above); this run is not a result", n)
	}
}

// ---- report

type benchKey struct{ row, cell, arm string }

func reportBench(t *testing.T, cfg benchConfig, header map[string]any, results []*benchSample) (unmeasured int) {
	var out *os.File
	if cfg.Out != "" {
		f, err := os.Create(cfg.Out)
		if err != nil {
			t.Errorf("CLEAT_BENCH_OUT %s: %v", cfg.Out, err)
		} else {
			out = f
			defer f.Close()
			b, _ := json.Marshal(header)
			fmt.Fprintln(out, string(b))
		}
	}
	var order []benchKey
	seen := map[benchKey]bool{}
	pooled := map[benchKey][]*benchSample{}
	for _, r := range results {
		k := benchKey{r.Row, r.Cell, r.Arm}
		if !seen[k] {
			seen[k] = true
			order = append(order, k)
		}
		pooled[k] = append(pooled[k], r)
		if out != nil {
			b, _ := json.Marshal(r)
			fmt.Fprintln(out, string(b))
		}
	}
	ms := func(d time.Duration) string { return fmt.Sprintf("%.1f", float64(d.Microseconds())/1000) }
	cells := map[string][]benchKey{}
	var cellOrder []string
	for _, k := range order {
		ck := k.row + "|" + k.cell
		if _, ok := cells[ck]; !ok {
			cellOrder = append(cellOrder, ck)
		}
		cells[ck] = append(cells[ck], k)
	}
	for _, ck := range cellOrder {
		first := cells[ck][0]
		t.Logf("BENCH [%s] %s", first.row, first.cell)
		medians := map[string]time.Duration{}
		repMeds := map[string][]time.Duration{}
		for _, k := range cells[ck] {
			reps := pooled[k]
			var all []time.Duration
			var repMed []time.Duration
			excluded, noisy := 0, 0
			loadMax := 0.0
			paths := map[string]int{}
			var why []string
			for _, r := range reps {
				all = append(all, r.Durations...)
				if len(r.Durations) > 0 {
					repMed = append(repMed, benchMedian(r.Durations))
				}
				excluded += r.Excluded
				if r.Noisy {
					noisy++
				}
				loadMax = max(loadMax, r.LoadBefore, r.LoadAfter)
				for p, n := range r.Paths {
					paths[p] += n
				}
				if r.Unmeasured != "" {
					why = append(why, fmt.Sprintf("rep %d: %s", r.Rep, r.Unmeasured))
					unmeasured++
				}
			}
			if k.row == "queries" {
				for _, r := range reps {
					if r.Extra != nil {
						t.Logf("BENCH   %-4s rep %d  statements/wait raw %.1f, minus idle %.1f  (idle %.1f/s over %.0fs, %v waits)  load %.2f→%.2f%s", k.arm, r.Rep,
							r.Extra["statements_per_wait_raw"], r.Extra["statements_per_wait_minus_idle"], r.Extra["idle_statements_per_second"], r.Extra["wall_seconds"], r.Extra["waits"], r.LoadBefore, r.LoadAfter, map[bool]string{true: "  NOISY", false: ""}[r.Noisy])
					}
				}
				for _, w := range why {
					t.Logf("BENCH   %-4s UNMEASURED %s", k.arm, w)
				}
				continue
			}
			if len(all) == 0 {
				t.Logf("BENCH   %-4s UNMEASURED  %s", k.arm, strings.Join(why, "; "))
				continue
			}
			s := benchSorted(all)
			medians[k.arm] = benchMedian(all)
			repMeds[k.arm] = repMed
			lo, hi := benchSorted(repMed)[0], benchSorted(repMed)[len(repMed)-1]
			note := ""
			if len(all) < 100 {
				note = "  (p99 is the maximum at this N)"
			}
			if excluded > 0 {
				note += fmt.Sprintf("  EXCLUDED %d trial(s) on the wrong path", excluded)
			}
			if noisy > 0 {
				note += fmt.Sprintf("  NOISY in %d of %d reps", noisy, len(reps))
			}
			if len(why) > 0 {
				note += "  PARTLY UNMEASURED: " + strings.Join(why, "; ")
			}
			t.Logf("BENCH   %-4s N=%-4d p50 %8s  p90 %8s  p99 %8s  min %8s  max %8s ms | rep medians %s..%s | paths %v | max load %.2f%s",
				k.arm, len(all), ms(benchMedian(all)), ms(benchPercentile(s, 0.90)), ms(benchPercentile(s, 0.99)), ms(s[0]), ms(s[len(s)-1]), ms(lo), ms(hi), paths, loadMax, note)
		}
		if on, ok1 := medians["on"]; ok1 {
			if on2, ok2 := medians["on2"]; ok2 {
				// Descriptive only. on and on2 are the same configuration, so this is how far two runs of one thing differ here; it is NOT a threshold for a real effect, because under no effect off and
				// on2 are exchangeable too, and |on-off| > |on-on2| then happens about half the time.
				t.Logf("BENCH   spread of one configuration (on vs on2, descriptive, not a threshold): medians differ by %s ms (%.1f%% of on)", ms(on2-on), 100*float64(on2-on)/float64(max(on, 1)))
			}
			if off, ok3 := medians["off"]; ok3 {
				t.Logf("BENCH   off minus on: %s ms (off is %.2fx on)", ms(off-on), float64(off)/float64(max(on, 1)))
			}
		}
		if line, ok := separation(repMeds["off"], append(append([]time.Duration(nil), repMeds["on"]...), repMeds["on2"]...)); ok {
			t.Logf("BENCH   %s", line)
		}
	}
	return unmeasured
}

// separation asks the question the rep medians can answer without a noise threshold: do ALL the off reps lie on one side of ALL the on and on2 reps? With R reps per arm, the R off medians
// against the 2R on and on2 medians, and no effect at all, the chance that the off ones are the R lowest or the R highest is 2 / C(3R, R): 1/42 at R = 3. It is a rank statement, so it does not
// care how loud the machine was, only whether the load happened to fall in the same order as the arms. It needs R >= 3 to be able to say anything.
func separation(off, rest []time.Duration) (string, bool) {
	r := len(off)
	if r < 3 || len(rest) != 2*r {
		return "", false
	}
	minOff, maxOff := benchSorted(off)[0], benchSorted(off)[r-1]
	sr := benchSorted(rest)
	below := maxOff < sr[0]
	above := minOff > sr[2*r-1]
	p := 2 / binomial(3*r, r)
	switch {
	case below:
		return fmt.Sprintf("separation: all %d off rep medians are BELOW all %d on/on2 rep medians (p = %.4f under no effect, 2/C(%d,%d))", r, 2*r, p, 3*r, r), true
	case above:
		return fmt.Sprintf("separation: all %d off rep medians are ABOVE all %d on/on2 rep medians (p = %.4f under no effect, 2/C(%d,%d))", r, 2*r, p, 3*r, r), true
	}
	return fmt.Sprintf("separation: NOT separated: the %d off rep medians interleave the %d on/on2 rep medians; no claim of a difference", r, 2*r), true
}

func binomial(n, k int) float64 {
	v := 1.0
	for i := 1; i <= k; i++ {
		v = v * float64(n-k+i) / float64(i)
	}
	return v
}

// The separation arithmetic has a known answer too: 1/42 at three reps, and the interleaved case must refuse.
func TestBenchSeparationIsRight(t *testing.T) {
	ms := func(v ...int) []time.Duration {
		var d []time.Duration
		for _, x := range v {
			d = append(d, time.Duration(x)*time.Millisecond)
		}
		return d
	}
	if got := binomial(9, 3); got != 84 {
		t.Errorf("C(9,3) = %v, want 84", got)
	}
	line, _ := separation(ms(80, 90, 100), ms(10, 20, 30, 15, 25, 35))
	if !strings.Contains(line, "ABOVE") || !strings.Contains(line, "p = 0.0238") {
		t.Errorf("off all above: %q, want ABOVE and p = 0.0238 (1/42)", line)
	}
	line, _ = separation(ms(1, 2, 3), ms(10, 20, 30, 15, 25, 35))
	if !strings.Contains(line, "BELOW") {
		t.Errorf("off all below: %q, want BELOW", line)
	}
	// Above the lowest on/on2 median but not above the highest: still interleaved. A check of only one end passes the case above and fails this one.
	line, _ = separation(ms(45, 90, 100), ms(20, 30, 40, 50, 60, 70))
	if !strings.Contains(line, "NOT separated") {
		t.Errorf("off above the lowest but not the highest: %q, want NOT separated", line)
	}
	line, _ = separation(ms(10, 90, 100), ms(20, 30, 40, 50, 60, 70))
	if !strings.Contains(line, "NOT separated") {
		t.Errorf("interleaved: %q, want NOT separated", line)
	}
	if _, ok := separation(ms(1, 2), ms(3, 4, 5, 6)); ok {
		t.Errorf("two reps cannot support a rank statement, but separation answered")
	}
}
