//go:build chaos

package chaos

import (
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// generate builds n scripts from a seed. The same seed gives the same scripts on every machine, and a failure prints the seed and the script
// index, so one command replays it. Every name in a script is unique (the oracle counts effects by name), and a script that waits on a signal
// has exactly one such wait.
func generate(seed int64, n int) []scenario {
	r := rand.New(rand.NewSource(seed))
	var out []scenario
	for i := 0; i < n; i++ {
		var ops []op
		next := 0
		name := func(prefix string) string { next++; return fmt.Sprintf("%s%d", prefix, next) }
		length := 3 + r.Intn(4) // 3..6 operations
		awaited := false
		for len(ops) < length {
			switch k := r.Intn(8); {
			case k <= 2:
				ops = append(ops, op{Op: "call", Name: name("C")})
			case k == 3:
				ops = append(ops, op{Op: "side_effect", Name: name("S")})
			case k == 4:
				ops = append(ops, op{Op: "sleep", Ms: int64(100 + r.Intn(400))})
			case k == 5:
				ops = append(ops, op{Op: "defer", Name: name("D")})
			case k == 6:
				att := 3 + r.Intn(3)
				ops = append(ops, op{Op: "call_retry", Name: name("R"), Attempts: att, Fail: 1 + r.Intn(2)})
			case k == 7 && !awaited:
				awaited = true
				ops = append(ops, op{Op: "await0", Name: "go", Hit: name("HIT"), Miss: name("MISS")})
			}
		}
		out = append(out, scenario{Name: fmt.Sprintf("gen-%d-%d", seed, i), Ops: ops})
	}
	return out
}

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return v
	}
	return def
}

type planned struct {
	sc     scenario
	g      goldenRecord
	b      request
	f      *fault
	verify func(scenario, goldenRecord, *fault, request, outcome) []string
}

// TestChaosSeeded runs generated scripts against every fault kind that applies to them.
//
//	CLEAT_CHAOS_SEED     the seed (default 1)
//	CLEAT_CHAOS_SCRIPTS  how many scripts to generate (default 3)
//	CLEAT_CHAOS_MODE     "sample" (default): a seeded random sample of CLEAT_CHAOS_SAMPLE fault runs (default 12), the bounded check;
//	                     "full": every boundary x every fault, the pre-release sweep
//	CLEAT_CHAOS_ONLY     one "<script index>/<fault>" to replay exactly, printed by a failure
func TestChaosSeeded(t *testing.T) {
	seed := int64(envInt("CLEAT_CHAOS_SEED", 1))
	scripts := generate(seed, envInt("CLEAT_CHAOS_SCRIPTS", 3))
	mode := os.Getenv("CLEAT_CHAOS_MODE")
	if mode == "" {
		mode = "sample"
	}
	e := newEnv(t)

	var plan []planned
	for _, sc := range scripts {
		g := goldenOf(t, e, sc)
		for _, b := range g.boundaries() {
			for _, mode := range []string{"before", "after"} {
				for _, k := range []struct {
					then   string
					verify func(scenario, goldenRecord, *fault, request, outcome) []string
				}{
					{"reclaim", verifyReclaim},
					{"terminate", verifyTerminate},
					{"dbwrite", verifyRefusedWriteStopsTheGuest},
					{"signal", verifyReclaim},
				} {
					if k.then == "signal" && !hasOp(sc, "await0") {
						continue
					}
					if !applies(k.then, mode, b, g) {
						continue
					}
					plan = append(plan, planned{sc: sc, g: g, b: b, f: &fault{Op: b.Op, Nth: b.Nth, Mode: mode, Then: k.then}, verify: k.verify})
				}
			}
		}
	}
	t.Logf("seed=%d scripts=%d: %d fault runs possible", seed, len(scripts), len(plan))
	for _, sc := range scripts {
		t.Logf("script %s: %s", sc.Name, describe(sc))
	}

	if only := os.Getenv("CLEAT_CHAOS_ONLY"); only != "" {
		var keep []planned
		for _, p := range plan {
			if p.sc.Name+"/"+p.f.String() == only {
				keep = append(keep, p)
			}
		}
		if len(keep) == 0 {
			t.Fatalf("CLEAT_CHAOS_ONLY=%q names no planned fault run for seed %d", only, seed)
		}
		plan = keep
	} else if mode == "sample" {
		// A seeded sample, in a stable order, so a failing sample reproduces.
		r := rand.New(rand.NewSource(seed ^ 0x5eed))
		r.Shuffle(len(plan), func(i, j int) { plan[i], plan[j] = plan[j], plan[i] })
		if n := envInt("CLEAT_CHAOS_SAMPLE", 12); n < len(plan) {
			plan = plan[:n]
		}
		sort.SliceStable(plan, func(i, j int) bool { return plan[i].sc.Name < plan[j].sc.Name })
	}

	for _, p := range plan {
		p := p
		t.Run(p.sc.Name+"/"+p.f.String(), func(t *testing.T) {
			repro := fmt.Sprintf("CLEAT_CHAOS_SEED=%d CLEAT_CHAOS_SCRIPTS=%d CLEAT_CHAOS_ONLY='%s/%s' go test -tags chaos ./tests/chaos -run TestChaosSeeded -v",
				seed, len(scripts), p.sc.Name, p.f)
			runFault(t, e, p.sc, p.g, p.f, p.b, p.verify, repro)
		})
	}
}

func hasOp(sc scenario, kind string) bool {
	for _, o := range sc.Ops {
		if o.Op == kind {
			return true
		}
	}
	return false
}

func describe(sc scenario) string {
	var parts []string
	for _, o := range sc.Ops {
		s := o.Op
		if o.Name != "" {
			s += ":" + o.Name
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " ")
}
