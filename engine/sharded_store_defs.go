package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"
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
//	purge runs its DELETE on every shard and holds each open, and commits them only once every one has
//	  succeeded (cleat#3687), so a shard that refuses rolls them all back and nothing is removed anywhere.
//	  That refusal is ROUTINE, not a race: workflow_payloads_def_fkey is NO ACTION and a FINISHED
//	  run keeps its row until retention, while version GC and `cleatctl versions purge` gate only on
//	  runs that are not settled. A shard holding history of the version refuses, and a purge that had
//	  already removed it from the shards before it would leave them without a definition their runs
//	  need (cleat#3546 review). The open DELETE keeps its row locked, so a run starting for the
//	  version meanwhile waits for the purge instead of landing between its writes. What is left is the
//	  COMMIT phase: the commits are one after another across databases, the designated shard is
//	  committed last and only if the rest succeeded, and a commit that fails after another shard has
//	  committed splits the purge. See PurgeWorkflowDef.
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

// defPurger is implemented by a store that can run a purge's DELETE and keep it uncommitted.
// PostgresStore does. A store that does not is purged by its own PurgeWorkflowDef at commit time,
// with nothing asked of it beforehand.
type defPurger interface {
	beginWorkflowDefPurge(ctx context.Context, name string, version int) (defPurgeHold, error)
}

// defPurgeHold is one shard's DELETE that has run and not been committed. rollback is safe after
// commit.
type defPurgeHold interface {
	commit() error
	rollback()
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

// lockDefFanOut takes the cross-shard lock for (name, version) (cleat#3547) and returns the function
// that releases it. Deploy, deprecate and purge all take the SAME lock, so a purge cannot remove a
// version from the shards between a deploy's writes to them, and two deprecations cannot leave the
// shards disagreeing about the flag. With one shard there is no cross-shard order to keep and the
// shard's own row lock is enough, so there is nothing to take; with none, the caller reports that.
// A multi-shard store whose designated shard cannot lock refuses, because running unordered would
// read as protected. verb is only for the message: "deploy", "deprecate" or "purge".
func (s *ShardedStore) lockDefFanOut(ctx context.Context, verb, name string, version int) (release func(), err error) {
	all, designated := s.defShards(name)
	if designated == nil || len(all) < 2 {
		return func() {}, nil
	}
	locker, ok := designated.Store.(defFanOutLocker)
	if !ok {
		return nil, fmt.Errorf("%s_workflow_def: shard %q cannot take the cross-shard deploy lock, so a %s to %d shards "+
			"could not be ordered against a concurrent one and would not be attempted; sharding is supported on PostgreSQL only",
			verb, designated.Config.Name, verb, len(all))
	}
	wait := s.defLockWait
	if wait <= 0 {
		wait = defaultDefFanOutLockWait
	}
	return locker.lockWorkflowDefFanOut(ctx, name, version, wait)
}

// DeployWorkflowDef writes the definition to every shard.
func (s *ShardedStore) DeployWorkflowDef(ctx context.Context, def *WorkflowDef) error {
	all, designated := s.defShards(def.Name)
	if designated == nil {
		return fmt.Errorf("deploy_workflow_def: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}

	// cleat#3547: one write to this (name, version) at a time across the shards, held until the last is
	// written. It is taken BEFORE the pre-pass, which is a check and not a lock, so the question it asks
	// is still true when the writes it permits begin.
	release, err := s.lockDefFanOut(ctx, "deploy", def.Name, def.Version)
	if err != nil {
		return err
	}
	defer release()

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

// MarkVersionDeprecated writes to every shard, under the same cross-shard lock as a deploy.
func (s *ShardedStore) MarkVersionDeprecated(ctx context.Context, name string, version int, deprecated bool) error {
	release, err := s.lockDefFanOut(ctx, "deprecate", name, version)
	if err != nil {
		return err
	}
	defer release()
	return s.fanOutDefWrite("mark_version_deprecated", name, version, true, func(sh *Shard) error {
		return sh.Store.MarkVersionDeprecated(ctx, name, version, deprecated)
	})
}

// PurgeWorkflowDef removes the definition from every shard, or from none if any shard refuses.
//
// It runs the DELETE on every shard that can hold one open, in the order it will commit (the other
// shards first, the name's own shard last), and commits them only once every DELETE has succeeded
// (cleat#3687). Until then each shard's row stays locked, so a run starting for this version on a
// shard waits for the purge and fails cleanly if the purge goes through; and a run that is already
// there makes that shard's DELETE fail, which rolls every shard back with nothing removed anywhere.
// A store that cannot hold a DELETE open is purged at commit time, as it was before.
//
// THE OPEN TRANSACTIONS ARE BOUNDED. The whole phase, from the first DELETE to the last commit, runs
// under one deadline, the cross-shard lock's wait (see lockDefFanOut), after which every shard still
// open is rolled back and the purge fails having removed nothing from any shard that had not
// committed. That is the same figure on purpose: a deploy queued behind this purge gives up at it, so
// holding the shards longer than that would only be holding them for a purge nobody is waiting on. A
// slow shard therefore cannot keep the others' rows locked beyond it. A purge costs one connection
// per shard for as long as it runs, and the lock's own connection on the name's own shard.
//
// WHAT IS LEFT, stated so nobody reads it as more: the COMMITS are still one after another across
// databases. A commit that fails on a shard after another has committed, or a process that dies
// between two commits, splits the purge exactly as before: the shards committed no longer hold the
// definition, the error names them, and deploying the version again restores them.
func (s *ShardedStore) PurgeWorkflowDef(ctx context.Context, name string, version int) error {
	all, designated := s.defShards(name)
	if designated == nil {
		return fmt.Errorf("purge_workflow_def: no shard available -- check shard configuration in CLEAT_SHARD_CONFIG")
	}
	// Before any DELETE, so a deploy cannot land between the shards' writes.
	release, err := s.lockDefFanOut(ctx, "purge", name, version)
	if err != nil {
		return err
	}
	defer release()

	// defShards puts the designated shard first; purge commits it last.
	order := make([]*Shard, 0, len(all))
	order = append(order, all[1:]...)
	order = append(order, all[0])

	bound := s.defLockWait
	if bound <= 0 {
		bound = defaultDefFanOutLockWait
	}
	phase, cancel := context.WithCancel(ctx)
	defer cancel()
	var timedOut atomic.Bool
	timer := time.AfterFunc(bound, func() { timedOut.Store(true); cancel() })
	defer timer.Stop()

	holds := make(map[*Shard]defPurgeHold, len(order))
	defer func() {
		for _, h := range holds {
			h.rollback()
		}
	}()
	gaveUp := func() error {
		return fmt.Errorf("purge_workflow_def %q version %d: gave up after %s with the deletes held open on the shards "+
			"(a shard was too slow); nothing that had not already committed was removed: %w", name, version, bound, context.DeadlineExceeded)
	}

	var refused []error
	for _, sh := range order {
		p, ok := sh.Store.(defPurger)
		if !ok {
			continue
		}
		h, err := p.beginWorkflowDefPurge(phase, name, version)
		if err != nil {
			if timedOut.Load() {
				return gaveUp()
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			refused = append(refused, fmt.Errorf("shard %q: %w", sh.Config.Name, err))
			continue
		}
		holds[sh] = h
	}
	if len(refused) > 0 {
		return fmt.Errorf("purge_workflow_def %q version %d refused, no shard was written: %w", name, version, errors.Join(refused...))
	}

	err = s.fanOutDefWrite("purge_workflow_def", name, version, false, func(sh *Shard) error {
		if h, ok := holds[sh]; ok {
			return h.commit()
		}
		return sh.Store.PurgeWorkflowDef(ctx, name, version)
	})
	if err != nil && timedOut.Load() {
		// The deadline can land between two commits. What was committed is in the error; say why the rest was not.
		return fmt.Errorf("%w (the %s bound on holding the deletes open ran out part way: %w)", err, bound, context.DeadlineExceeded)
	}
	return err
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
