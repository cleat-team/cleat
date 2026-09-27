package engine

// CompiledSizeEstimateMultiplier is how much larger compiled native code is
// than the wasm it came from.
//
// MEASURED, on PostgreSQL-independent ground: each artifact below compiled with
// wasmtime.NewConfig() + SetEpochInterruption(true) -- the configuration
// NewWasmtimeBackend uses -- and sized with len(Serialize()). runtime.MemStats
// around each compile tracked the serialized figure closely, so it is a fair
// proxy for resident cost rather than an artifact of serialisation.
//
//	widget-store (AssemblyScript)      9,496 ->     86,400   9.1x
//	as-workflow (AssemblyScript)      26,367 ->    141,904   5.4x
//	rust-all-host-calls (release)    146,760 ->    442,432   3.0x
//	rust-workflow (release)          149,027 ->    457,952   3.1x
//	hostcallsrust (release)          252,411 ->    679,152   2.7x
//	java-workflow                    299,189 ->    694,184   2.3x
//	javaworkflow (prebuilt)          342,169 ->    748,008   2.2x
//	saga-java-port                   369,459 ->    841,336   2.3x
//	hostcallsjava                    506,181 ->  1,044,072   2.1x
//	call_all_plugins (Python)     19,300,914 -> 46,019,864   2.4x
//	rust-workflow (DEBUG)          5,244,200 ->  1,113,200   0.2x
//
// # Why 4, when every release build measures 2.1x-3.1x
//
// BECAUSE 3 WAS NOT ENOUGH, and the guard that re-derives these ratios is what
// said so. The first version of this constant was 3, chosen by reading the
// table above as "2.1x to 3.1x, call it 3" -- and
// TestTheCompiledSizeEstimateStillHolds immediately failed on the two
// artifacts that sit AT the top of that range:
//
//	rust-workflow (release)          149,059 -> 457,952   3.1x, over by 10 KB
//	rust-all-host-calls (release)    146,792 -> 442,432   3.0x, over by  2 KB
//
// An estimate that under-counts is the one failure this bound cannot tolerate:
// the cache would hold more than a deployment was told, silently, which is
// cleat#1907's own complaint one layer down. So the multiplier clears the
// measured maximum rather than approximating it.
//
// The cost of 4 is that a typical release artifact is over-counted by about a
// third, so a given --wasm-module-cache-max-mb holds correspondingly fewer
// modules than its raw arithmetic suggests. That is the safe direction, and it
// is stated here so nobody later "tightens" it back to 3 by reading the same
// table the same way.
//
// The outliers stay outside and both are safe. The ASSEMBLYSCRIPT artifacts run
// 5.4x and 9.1x and are 86 KB and 142 KB, so under-counting them loses tens of
// kilobytes -- the guard excludes anything under 100 KB for exactly that
// reason. The DEBUG build runs 0.2x because debug wasm carries symbols that
// never become code, so the estimate over-counts it further.
//
// At the size where precision matters -- the 19 MB Python component, the
// artifact that makes a hundred entries mean gigabytes -- the ratio is 2.4x.
//
// # Why an estimate rather than the real number
//
// Serialize() gives the exact size and costs a serialisation per insert, plus
// a transient allocation the size of the compiled module: 46 MB for the Python
// artifact, produced and discarded to learn a number the input length already
// predicts within a factor this bound tolerates.
//
// cleat#1907. TestTheCompiledSizeEstimateStillHolds re-derives the ratios from
// the checked-in artifacts and fails on a changed RATIO rather than a changed
// absolute size, because the artifacts get rebuilt.
const CompiledSizeEstimateMultiplier = 4

// DefaultModuleCacheMaxBytes is the default ceiling on the compiled-module
// cache's estimated resident size.
//
// 512 MB, chosen against the measurements above rather than as a round number:
// at the 4x estimate it holds roughly six Python components, or every
// release-built Rust and Java artifact a deployment is likely to have many
// times over. A
// deployment that runs many large Python guests is the one that has to raise
// it, and it is the one for which the old entry-count bound was silently worth
// gigabytes.
const DefaultModuleCacheMaxBytes int64 = 512 << 20

// CompiledSizeEstimate returns the estimated resident cost of compiling
// wasmLen bytes of wasm.
//
// Returns 0 for a non-positive length, which is how a caller with no source
// length in hand contributes nothing to the byte bound rather than an invented
// figure. That caller is still bounded by the entry count.
func CompiledSizeEstimate(wasmLen int) int64 {
	if wasmLen <= 0 {
		return 0
	}
	return int64(wasmLen) * CompiledSizeEstimateMultiplier
}
