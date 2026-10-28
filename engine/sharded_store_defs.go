package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Workflow definitions are REPLICATED to every shard (cleat#3545, owner decision D42).
//
// WHY. A run's rows are placed by getShard(runID) and its definition used to be placed by
// getShard(def.Name). workflow_payloads_def_fkey is a foreign key inside ONE database, so a run
// on any shard but its definition's own failed to start. Placing runs by definition name instead
// would have put every run of a busy workflow on one shard, which is the load the shards exist to
// spread; copying the definition does not.
//
// THE RULES, one per kind of access:
//
//	a definition WRITE (deploy, deprecate, purge) goes to every shard
//	a run reads the definition on ITS OWN shard (the foreign key makes that the only place it can
//	  be; no code here is needed for it)
//	a lookup BY NAME reads the DESIGNATED shard, getShard(name), first. That is the shard the
//	  tag and routing-rule tables live on, because they are read and written by name and their
//	  foreign keys point at the definition on that same database, so reading the designated shard
//	  is what keeps a name lookup in agreement with the fan-out
//	a COUNT of runs sums every shard, because the runs are spread over all of them
//
// THERE IS NO CROSS-DATABASE TRANSACTION, so a fan-out can stop half way. What this does about it:
//
//	deploy and deprecate write the designated shard first and stop if it fails, so a failure there
//	  leaves nothing changed
//	purge asks every shard first (a DELETE that is rolled back) and writes nothing if any refuses.
//	  That refusal is ROUTINE, not a race: workflow_payloads_def_fkey is NO ACTION and a FINISHED
//	  run keeps its row until retention, while version GC and `cleatctl versions purge` gate only on
//	  runs that are not settled. A shard holding history of the version refuses, and a purge that had
//	  already removed it from the shards before it would leave them without a definition their runs
//	  need (cleat#3546 review). Only a run that starts between the question and the write can still
//	  split a purge, and then the designated shard is written last and only if the rest succeeded
//	every write is idempotent
//	the error names the shards that were written, the shards that failed and the shards not tried,
//	  and says what repairs that operation
//
// A deploy over bytes that differ from the deployed ones is refused when ANY shard has open runs of
// that version (cleat#3497). Each shard's own write checks only its own runs, so without a
// read-only pass over all of them first, a replacement could land on a shard with none and be
// refused on the next, leaving the shards holding different code for one version.

// ShardedDefWriteError reports a definition write that did not reach every shard.
type ShardedDefWriteError struct {
	Op           string
	Name         string
	Version      int
	Succeeded    []string
	Failed       []string
	NotAttempted []string
	Errs         []error
	// Repair says what to do about it, which differs by operation: running a refused purge again is
	// refused again.
	Repair string
}

func (e *ShardedDefWriteError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %q version %d did not reach every shard: written to [%s], failed on [%s]",
		e.Op, e.Name, e.Version, strings.Join(e.Succeeded, ", "), strings.Join(e.Failed, ", "))
	if len(e.NotAttempted) > 0 {
		fmt.Fprintf(&b, ", not attempted on [%s]", strings.Join(e.NotAttempted, ", "))
	}
	b.WriteString(". ")
	b.WriteString(e.Repair)
	for _, err := range e.Errs {
		b.WriteString("; ")
		b.WriteString(err.Error())
	}
	return b.String()
}

// Unwrap lets errors.Is and errors.As see each shard's own error.
func (e *ShardedDefWriteError) Unwrap() []error { return e.Errs }

// defReplacementPreflight is implemented by a store that can ask cleat#3497's question without
// writing. PostgresStore does. A store that does not is still written to, and its own write applies
// the rule on that shard alone.
type defReplacementPreflight interface {
	preflightWorkflowDefReplacement(ctx context.Context, def *WorkflowDef) error
}

// defPurgePreflight is implemented by a store that can ask whether a purge would be refused without
// removing anything. PostgresStore does, by running the DELETE and rolling it back, so the question
// is the statement itself and cannot drift from it.
type defPurgePreflight interface {
	preflightWorkflowDefPurge(ctx context.Context, name string, version int) error
}

// defShards returns every shard with the designated one for name first, and the designated one
// alone as the second result.
func (s *ShardedStore) defShards(name string) (all []*Shard, designated *Shard) {
	designated = s.getShard(name)
	s.mu.RLock()
	shards := s.shards
	s.mu.RUnlock()
	all = make([]*Shard, 0, len(shards))
	if designated != nil {
		all = append(all, designated)
	}
	for _, sh := range shards {
		if sh != designated {
			all = append(all, sh)
		}
	}
	return all, designated
}

// fanOutDefWrite applies write to every shard. designatedFirst orders the designated shard first and
// stops on its failure (deploy, deprecate); otherwise it is last and is written only if every other
// shard succeeded (purge).
func (s *ShardedStore) fanOutDefWrite(op, name string, version int, designatedFirst bool, write func(*Shard) error) error {
	all, designated := s.defShards(name)
	if designated == nil {
		return fmt.Errorf("%s: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG", op)
	}
	if !designatedFirst {
		// defShards puts the designated shard first; move it to the end.
		reordered := make([]*Shard, 0, len(all))
		reordered = append(reordered, all[1:]...)
		all = append(reordered, all[0])
	}
	repair := "Run the same command again: every shard's write is idempotent, so it repairs this"
	if !designatedFirst {
		repair = "The shards written no longer hold this definition: deploy the version again to restore them, " +
			"or clear what the failing shard holds and purge again"
	}
	res := &ShardedDefWriteError{Op: op, Name: name, Version: version, Repair: repair}
	for i, sh := range all {
		isDesignated := sh == designated
		if !designatedFirst && isDesignated && len(res.Failed) > 0 {
			res.NotAttempted = append(res.NotAttempted, sh.Config.Name)
			continue
		}
		if err := write(sh); err != nil {
			res.Failed = append(res.Failed, sh.Config.Name)
			res.Errs = append(res.Errs, fmt.Errorf("shard %q: %w", sh.Config.Name, err))
			if designatedFirst && isDesignated {
				for _, rest := range all[i+1:] {
					res.NotAttempted = append(res.NotAttempted, rest.Config.Name)
				}
				break
			}
			continue
		}
		res.Succeeded = append(res.Succeeded, sh.Config.Name)
	}
	if len(res.Failed) == 0 {
		return nil
	}
	// Nothing was written anywhere: the caller's own error, undecorated by a "written to []" report.
	if len(res.Succeeded) == 0 && len(res.Errs) == 1 {
		return res.Errs[0]
	}
	return res
}

// DeployWorkflowDef writes the definition to every shard.
func (s *ShardedStore) DeployWorkflowDef(ctx context.Context, def *WorkflowDef) error {
	all, designated := s.defShards(def.Name)
	if designated == nil {
		return fmt.Errorf("deploy_workflow_def: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}

	// cleat#3497 across every shard, before any is written. See the comment at the top of the file.
	openRuns := 0
	for _, sh := range all {
		pf, ok := sh.Store.(defReplacementPreflight)
		if !ok {
			continue
		}
		err := pf.preflightWorkflowDefReplacement(ctx, def)
		var inUse *WorkflowDefInUseError
		switch {
		case err == nil:
		case errors.As(err, &inUse):
			openRuns += inUse.OpenRuns
		default:
			return fmt.Errorf("deploy_workflow_def: shard %q: %w (no shard was written)", sh.Config.Name, err)
		}
	}
	if openRuns > 0 {
		return &WorkflowDefInUseError{Name: def.Name, Version: def.Version, OpenRuns: openRuns}
	}

	return s.fanOutDefWrite("deploy_workflow_def", def.Name, def.Version, true, func(sh *Shard) error {
		return sh.Store.DeployWorkflowDef(ctx, def)
	})
}

// MarkVersionDeprecated writes to every shard.
func (s *ShardedStore) MarkVersionDeprecated(ctx context.Context, name string, version int, deprecated bool) error {
	return s.fanOutDefWrite("mark_version_deprecated", name, version, true, func(sh *Shard) error {
		return sh.Store.MarkVersionDeprecated(ctx, name, version, deprecated)
	})
}

// PurgeWorkflowDef removes the definition from every shard, or from none if any shard that can say
// so beforehand would refuse.
func (s *ShardedStore) PurgeWorkflowDef(ctx context.Context, name string, version int) error {
	all, designated := s.defShards(name)
	if designated == nil {
		return fmt.Errorf("purge_workflow_def: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	var refused []error
	for _, sh := range all {
		pf, ok := sh.Store.(defPurgePreflight)
		if !ok {
			continue
		}
		if err := pf.preflightWorkflowDefPurge(ctx, name, version); err != nil {
			refused = append(refused, fmt.Errorf("shard %q: %w", sh.Config.Name, err))
		}
	}
	if len(refused) > 0 {
		return fmt.Errorf("purge_workflow_def %q version %d refused, no shard was written: %w", name, version, errors.Join(refused...))
	}
	return s.fanOutDefWrite("purge_workflow_def", name, version, false, func(sh *Shard) error {
		return sh.Store.PurgeWorkflowDef(ctx, name, version)
	})
}

// ListWorkflowDefs aggregates every shard and returns each (name, version) once. Every shard holds a
// copy of every definition, so a plain concatenation would list each N times. Where copies differ,
// which only a partly-applied write can cause, the designated shard's copy wins, because that is
// the one a lookup by name reads.
func (s *ShardedStore) ListWorkflowDefs(ctx context.Context, name string) ([]WorkflowDef, error) {
	type key struct {
		name    string
		version int
	}
	s.mu.RLock()
	shards := s.shards
	s.mu.RUnlock()
	byKey := make(map[key]WorkflowDef)
	for _, sh := range shards {
		defs, err := sh.Store.ListWorkflowDefs(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("shard %q: %w", sh.Config.Name, err)
		}
		for _, d := range defs {
			k := key{d.Name, d.Version}
			if _, seen := byKey[k]; seen && s.getShard(d.Name) != sh {
				continue
			}
			byKey[k] = d
		}
	}
	all := make([]WorkflowDef, 0, len(byKey))
	for _, d := range byKey {
		all = append(all, d)
	}
	// What each shard's own ORDER BY gives: name ascending, version descending.
	sort.Slice(all, func(i, j int) bool {
		if all[i].Name != all[j].Name {
			return all[i].Name < all[j].Name
		}
		return all[i].Version > all[j].Version
	})
	return all, nil
}

// CountActiveInstances sums every shard: the runs of one version are spread over all of them, and
// a count of one shard's would let version GC and `cleatctl versions purge` call a version unused
// while another shard still has it running.
func (s *ShardedStore) CountActiveInstances(ctx context.Context, name string, version int) (int, error) {
	s.mu.RLock()
	shards := s.shards
	s.mu.RUnlock()
	if len(shards) == 0 {
		return 0, fmt.Errorf("count_active_instances: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	total := 0
	for _, sh := range shards {
		n, err := sh.Store.CountActiveInstances(ctx, name, version)
		if err != nil {
			return 0, fmt.Errorf("count_active_instances: shard %q: %w", sh.Config.Name, err)
		}
		total += n
	}
	return total, nil
}
