package engine

import "context"

import "github.com/tetratelabs/wazero/api"

// replayRun is step's replay half for a MULTI-RECORD step (a spec with a Role): one call whose history is a run of consecutive records, any
// number of rolePrefix records ending in a roleFinal one (cleat#4152, W2a; consumers: a retried call, SetScope, the awaits).
//
// It PEEKS the run first and consumes nothing until the run is known to be valid, so the one rule of the dispatcher holds for a run as it does
// for a single record: a divergence consumes nothing (cleat#4157). Reading a run, from the first record:
//
//	foreign first                  a divergence naming that record; nothing consumed.
//	prefix* final                  complete: every record is consumed and DecodeRun answers.
//	prefix+ then END OF HISTORY    the history ended INSIDE the run (a run lost mid-policy): every prefix record is consumed, replay ends, and
//	                               Fresh continues with c.Prefix. endedInside is true and step() does the leaving.
//	prefix+ then a foreign record  complete WITHOUT a final if the spec says EndsWithoutFinal (an await recorded and never answered: it timed
//	                               out); otherwise a divergence naming the foreign record, nothing consumed.
//	an unknown Role value          a divergence naming that record, ALWAYS: a record the spec does not classify is a bug in the spec, and
//	                               EndsWithoutFinal does not make it a legitimate end.
//
// Identity is applied to every record of the run before anything is consumed, so a record that names another key or service is a divergence
// whichever position it holds (cleat#4211; a first record used to escape the comparison). ONE exception, for a spec with EndsWithoutFinal: a
// record that fails Identity AFTER at least one prefix record is the next call's, not this one's, and ends the run. Two awaits in a row, each
// recorded and never answered, are [pending a, pending b, ...]: by type alone step a's run would swallow b's record, so for such a spec Identity
// is what finds the end of a run (validate() requires one). A mismatch on the FIRST record is still a divergence: this call has nothing there.
//
// It never calls leaveReplay or Record: leaving replay is step's alone, which is what the dispatcher's caller tests assert, and a run's records
// are written only by a spec's Fresh through c.Record.
func (s *execSession) replayRun(ctx context.Context, m api.Module, sp *stepSpec, c *stepCtx) (res int64, endedInside bool) {
	args := c.args
	var run []EventRecord
	final := false
	prefixes := 0
	foreign := -1
	unknown := -1 // a Role value that is none of the three: kept apart from foreign, because a foreign record can legitimately end a run and this cannot
read:
	for i := s.stepCount; i < len(s.history); i++ {
		rec := s.history[i]
		role := sp.Role(args, &rec)
		switch role {
		case rolePrefix, roleFinal:
			// For a spec whose run can end without a final, Identity finds the end: a record that names another call, after at least one record
			// of this run, belongs to the next call and is not read here.
			if sp.EndsWithoutFinal && len(run) > 0 && sp.Identity != nil && sp.Identity(args, &rec) != "" {
				foreign = i
				break read
			}
			// A spec that bounds its prefix records (an await holds one) treats one past the bound as the next call's.
			if role == rolePrefix && sp.MaxPrefix > 0 && prefixes >= sp.MaxPrefix {
				foreign = i
				break read
			}
			if role == rolePrefix {
				prefixes++
			}
			run = append(run, rec)
			if role == roleFinal {
				final = true
				break read
			}
		case roleForeign:
			foreign = i
			break read
		default:
			unknown = i
			break read
		}
	}
	if len(run) == 0 && foreign == s.stepCount && sp.ForeignFirst != nil {
		if res, ok := sp.ForeignFirst(args); ok {
			return res, false
		}
	}
	if len(run) == 0 {
		return s.replayDivergenceAt(ctx, m, &s.history[s.stepCount], sp.Want, args.outPtr, args.outMaxLen), false
	}
	if sp.Identity != nil {
		for k := range run {
			if why := sp.Identity(args, &run[k]); why != "" {
				return s.replayIdentityDivergenceAt(ctx, m, &run[k], sp.Name, why, args.outPtr, args.outMaxLen), false
			}
		}
	}
	if unknown >= 0 {
		return s.replayDivergenceAt(ctx, m, &s.history[unknown], sp.Want, args.outPtr, args.outMaxLen), false
	}
	if !final && foreign >= 0 && !sp.EndsWithoutFinal {
		return s.replayDivergenceAt(ctx, m, &s.history[foreign], sp.Want, args.outPtr, args.outMaxLen), false
	}
	for k := range run {
		rec := run[k]
		if !s.advanceReplayStep(ctx, &rec) {
			return 0, false
		}
	}
	if !final && foreign < 0 {
		c.Prefix = run
		return 0, true
	}
	return sp.DecodeRun(c, run), false
}
