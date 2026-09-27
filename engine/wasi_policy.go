package engine

// WASI preview1 has no policy of its own: a host either offers the whole
// interface or none of it. cleat offered the whole of it -- 46 functions, to
// every guest, on both backends -- while 16 are demanded by any shipped guest
// and the unreachable remainder includes the entire path_* and sock_* families.
// cleat#1381 asked which of them a durable-execution guest should be able to
// reach; the answer recorded there is an allowlist, and this table is it.
//
// WHAT BOUNDED THE UNUSED FUNCTIONS BEFORE THIS TABLE, and why it was not a
// policy: path_* and sock_* need a file descriptor to act on and cleat
// provisions none -- no WithFSConfig on wazero, no PreopenDir on wasmtime. So
// path_open failed for want of a preopened directory rather than because
// anything refused it. That is containment by configuration, one line away from
// being lost: a preopen added for a plugin would silently open a family of
// calls nobody had reasoned about. The functions were already bound; only their
// inputs were empty.
//
// A GUEST CAN CALL ONLY WHAT IT IMPORTS, which is what makes the refusals
// cheap: refusing a function no guest imports is unobservable. It is also why
// the refusal is a TRAP ON CALL rather than a refusal to load -- a guest that
// merely declares an import it never reaches keeps working.
//
// THE ALLOWLIST IS THE 16 FUNCTIONS SHIPPED GUESTS DEMAND, and the refused 30
// are exactly the ones nothing demands. cleat#1381 proposed refusing a
// seventeenth, poll_oneoff; that was implemented, measured, and reverted. Its
// entry below records why, and it is the one piece of this table worth reading
// before changing anything.
type wasiBucket int

const (
	// wasiDeterministic: same inputs, same result, no external state. Passed
	// through to the backend's own WASI implementation.
	wasiDeterministic wasiBucket = iota

	// wasiIntercepted: replaced by a recorded, replayable source. cleat#1300.
	wasiIntercepted

	// wasiFatal: refused loudly, because there is no replayable meaning.
	wasiFatal
)

func (b wasiBucket) String() string {
	switch b {
	case wasiDeterministic:
		return "deterministic"
	case wasiIntercepted:
		return "intercepted"
	case wasiFatal:
		return "fatal"
	}
	return "unknown"
}

// wasiPolicyEntry is one function's bucket and the reason it is in it. The
// reason is not decoration: the buckets for args_*, environ_* and fd_read are
// statements about cleat's CONFIGURATION rather than about the functions, and a
// reader who cannot tell those apart from the genuine ones will move the wrong
// entry when the configuration changes.
type wasiPolicyEntry struct {
	bucket wasiBucket
	reason string
}

// wasiPreview1Policy is the stated list. Every function the host offers must
// appear here exactly once; TestTheWasiPolicyCoversEveryOfferedFunction fails
// if the offered set and this table's keys differ in either direction, so a
// dependency bump that adds a 47th function turns red rather than defaulting it
// to something by accident.
var wasiPreview1Policy = map[string]wasiPolicyEntry{
	// ---- demanded by shipped guests, and allowed (15) ----------------------
	"args_get": {wasiDeterministic,
		"cleat sets no args, so this returns empty. A fact about the configuration, " +
			"not about the function: it stops being true the moment anything passes argv."},
	"args_sizes_get": {wasiDeterministic, "as args_get."},
	"clock_time_get": {wasiIntercepted,
		"replaced by the durable clock (realtime) and a synthetic counter (monotonic). " +
			"cleat#1300; see registerWasiDeterminism for why the two domains are sourced apart."},
	"environ_get": {wasiDeterministic,
		"cleat calls nothing that populates the environment, so this returns empty. " +
			"A configuration fact, as args_get -- InheritEnv would invalidate it."},
	"environ_sizes_get": {wasiDeterministic, "as environ_get."},
	"fd_close": {wasiDeterministic,
		"descriptor bookkeeping over an empty descriptor table; consistent answers."},
	"fd_fdstat_get":       {wasiDeterministic, "descriptor introspection; as fd_close."},
	"fd_fdstat_set_flags": {wasiDeterministic, "descriptor introspection; as fd_close."},
	"fd_prestat_dir_name": {wasiDeterministic, "reports the preopens, of which cleat provisions none."},
	"fd_prestat_get":      {wasiDeterministic, "as fd_prestat_dir_name."},
	"fd_read": {wasiDeterministic,
		"deterministic BY ACCIDENT: no stdin is configured, so it reports EOF. The same " +
			"fragility as environ_* -- this describes cleat's configuration, not WASI."},
	"fd_write": {wasiDeterministic,
		"THE WEAKEST ENTRY IN THIS TABLE, and deliberately allowed anyway. It writes to " +
			"the host's stderr under InheritStderr: not state, but an unrecorded side effect " +
			"that repeats on every replay, so it is not deterministic in the sense the other " +
			"rows are. cleat's replayable path is DurableLog. It is allowed because refusing " +
			"it removes a guest's panic output, which is the one thing you need when a guest " +
			"fails. cleat#1381 left this as 'intercept or accept'; accepting is the reversible " +
			"half of that and routing it through the host logger is the open follow-up."},
	"proc_exit":   {wasiDeterministic, "already how a guest terminates."},
	"random_get":  {wasiIntercepted, "replaced by the seeded source cleat_random uses. cleat#1300."},
	"sched_yield": {wasiDeterministic, "a hint with no observable result."},

	// poll_oneoff is ALLOWED, and cleat#1381 proposed refusing it. The proposal
	// was tried here and reverted, because the measurement behind it does not
	// generalise the way it appears to.
	//
	// WHAT THE ISSUE MEASURED IS TRUE: it does not block. It spins on nanotime
	// until the deadline passes, and under a clock that does not advance it
	// spins forever -- 51,177,005 host calls in twenty seconds for a 500ms
	// sleep (cleat#1300). A guest timer is also unrecorded and re-executes on
	// replay, and cleat's answer to sleeping is DurableSleepMs.
	//
	// WHAT MAKES IT UNREFUSABLE ANYWAY: the Go runtime calls it, on paths that
	// have nothing to do with a guest asking to sleep. Refusing it broke
	// TestAGuestKilledByTheMemoryLimitIsNotReportedAsSuccess and
	// TestTheHostRunsDefersOfAnOOMKilledWorkflow -- an out-of-memory guest
	// parks a goroutine through poll_oneoff, the trap replaced the memory
	// failure with a WASI refusal, and the host's OOM classification no longer
	// recognised it and reported the workflow as SUCCEEDING. That is a
	// regression of IMPROVEMENT-PLAN 3.71, traded for refusing a function every
	// Go guest needs.
	//
	// AND NOTE HOW THE EVIDENCE FOR REFUSING IT WAS COLLECTED, because it is
	// the more useful half. A probe counted poll_oneoff calls across three
	// ordinary runs of testdata/basic -- a success, a guest error, and a
	// three-call long_running -- and found zero every time. "Imported but never
	// called" was a true statement about the population measured and a false
	// one about the function: none of those three runs was under memory
	// pressure, which is the condition that reaches it. A happy-path census
	// cannot see a path taken only when something is going wrong.
	//
	// It terminates in practice because the monotonic clock advances
	// wasiMonotonicStepNs per read; the non-advancing case in cleat#1300 is not
	// the configuration cleat ships. The determinism concern is real and it
	// belongs to the CLOCK (cleat#1300), which is intercepted, rather than to
	// this allowlist.
	"poll_oneoff": {wasiDeterministic,
		"called by the Go runtime's scheduler, not only by guest sleeps; refusing it " +
			"makes an OOM-killed workflow report success. See the comment above."},

	// ---- offered, demanded by nothing, refused (30) ------------------------
	// Refusing these costs nothing today and would be a breaking change later,
	// which is the argument for doing it now. clock_res_get is the one the
	// issue proposed as deterministic; it is refused here because no guest
	// demands it and the recorded decision defaults the undemanded to fatal.
	// Moving it is a one-line change if a guest ever needs it.
	"clock_res_get":           {wasiFatal, "undemanded. cleat#1381 proposed deterministic; the decision defaults the undemanded to fatal."},
	"fd_advise":               {wasiFatal, "positional/file I/O has no replayable meaning in a workflow."},
	"fd_allocate":             {wasiFatal, "positional/file I/O has no replayable meaning in a workflow."},
	"fd_datasync":             {wasiFatal, "positional/file I/O has no replayable meaning in a workflow."},
	"fd_fdstat_set_rights":    {wasiFatal, "rights amplification over a descriptor table cleat does not populate."},
	"fd_filestat_get":         {wasiFatal, "filesystem metadata; external state."},
	"fd_filestat_set_size":    {wasiFatal, "filesystem mutation."},
	"fd_filestat_set_times":   {wasiFatal, "filesystem mutation."},
	"fd_pread":                {wasiFatal, "positional I/O has no replayable meaning in a workflow."},
	"fd_pwrite":               {wasiFatal, "positional I/O has no replayable meaning in a workflow."},
	"fd_readdir":              {wasiFatal, "directory enumeration; external state."},
	"fd_renumber":             {wasiFatal, "descriptor table mutation."},
	"fd_seek":                 {wasiFatal, "positional I/O has no replayable meaning in a workflow."},
	"fd_sync":                 {wasiFatal, "filesystem durability; cleat's durability is the event history."},
	"fd_tell":                 {wasiFatal, "positional I/O has no replayable meaning in a workflow."},
	"path_create_directory":   {wasiFatal, "filesystem mutation."},
	"path_filestat_get":       {wasiFatal, "filesystem metadata; external state."},
	"path_filestat_set_times": {wasiFatal, "filesystem mutation."},
	"path_link":               {wasiFatal, "filesystem mutation."},
	"path_open":               {wasiFatal, "filesystem access; the family this policy most exists to refuse."},
	"path_readlink":           {wasiFatal, "filesystem metadata; external state."},
	"path_remove_directory":   {wasiFatal, "filesystem mutation."},
	"path_rename":             {wasiFatal, "filesystem mutation."},
	"path_symlink":            {wasiFatal, "filesystem mutation."},
	"path_unlink_file":        {wasiFatal, "filesystem mutation."},
	"proc_raise":              {wasiFatal, "signals the host process; not a workflow's business."},
	"sock_accept":             {wasiFatal, "network I/O is not replayable; cleat's answer is DurableCall."},
	"sock_recv":               {wasiFatal, "network I/O is not replayable; cleat's answer is DurableCall."},
	"sock_send":               {wasiFatal, "network I/O is not replayable; cleat's answer is DurableCall."},
	"sock_shutdown":           {wasiFatal, "network I/O is not replayable; cleat's answer is DurableCall."},
}

// wasiIsFatal reports whether a WASI preview1 function is refused.
//
// An UNKNOWN NAME IS FATAL. That is the recorded default and it is the half
// that matters: a function this table has never heard of is exactly the case
// the policy exists for, and defaulting it to allowed would make the table a
// denylist wearing an allowlist's name.
func wasiIsFatal(name string) bool {
	e, ok := wasiPreview1Policy[name]
	return !ok || e.bucket == wasiFatal
}
