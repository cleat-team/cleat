//go:build cgo

package engine

import (
	"sort"
	"strings"
	"testing"
)

// TestEveryHostStopSiteIsClassifiedForTheStoppableSet keeps stoppableFirstEvents in step with the host's stop sites
// (cleat#4079). A defer segment over a killed run replays the body up to the last event in that set and stops there, which
// is only safe if the host will REFUSE the call that event began: otherwise the guest's next call runs real work inside a
// segment that exists to run cleanup. The set was first written by hand and, when this test was added, four of the host's
// refusing sites (acquire_lock, schedule_cron, delete_cron, durable_schedule_invoke) were missing from it and three events
// that no site refuses (await_child, await_all_children, continue_as_new) were in it. Nothing failed either way.
//
// The sites come from hostStopSites, the same source of truth TestTheThreeStopSurfacesAgree uses, so a new refusing
// site arrives here as an unclassified name and fails this test instead of being silently left out of the set.

// stopEvent names the first event a call records: its type and, where the type is shared with a read, the Op that marks it as the
// effect (stoppableByOp).
type stopEvent struct {
	typ EventType
	op  string
}

// stopSiteEvents maps a host stop site to the FIRST event its call records. Every entry must be a stop point: its type in
// stoppableFirstEvents, or its (type, op) in stoppableByOp.
var stopSiteEvents = map[string]stopEvent{
	"DurableCall":              {typ: EventTypeCall},
	"DurableCallWithRetry":     {typ: EventTypeCall},
	"DurableCallWithHeartbeat": {typ: EventTypeCall},
	"PluginCall":               {typ: EventTypePluginCall},
	"PluginCallStreaming":      {typ: EventTypePluginCall},
	"SideEffect":               {typ: EventTypeSideEffect},
	"Fetch":                    {typ: EventTypeFetch},
	"DurableDefer":             {typ: EventTypeDefer},
	"DurableSend":              {typ: EventTypeDurableSend},
	"DurableScheduleInvoke":    {typ: EventTypeDurableScheduleInvoke},
	"ScheduleCron":             {typ: EventTypeScheduleCron},
	"freshAcquireLock":         {typ: EventTypeAcquireLock},
	"ResolvePromise":           {typ: EventTypePromiseResolved, op: PromiseOpResolve},
	"RejectPromise":            {typ: EventTypePromiseRejected, op: PromiseOpReject},
	"childWorkflowWithVersion": {typ: EventTypeChildWorkflow},
	"runDetached":              {typ: EventTypeRunDetached},
	"DurableAwaitSignals":      {typ: EventTypeAwaitSignals},
}

// stopSiteNotStoppable lists the sites whose event cannot be a stop point, each with the reason. A reason is the mechanism, so
// that a change to it makes the entry stale on purpose.
var stopSiteNotStoppable = map[string]string{
	"SignalWorkflow": "it records signal_received, the same event type signal DELIVERY records, so a stop point keyed on " +
		"that type could land on a delivered signal, which no call of the guest refuses",
	"DurablePollUpdate": "it records update_received, the same event type that records an update's DELIVERY, for the same " +
		"reason as signal_received",
	"DeleteCron": "the host refuses it in a segment (cleat#4083), but its WIT function returns nothing (reasonWitReturnsNothing in " +
		"stopSurfaces), so a component guest never receives the stop and carries on past the refused call, back into the loop the kill " +
		"interrupted",
	"CreatePromise": "the host refuses it in a segment (cleat#4091), but its WIT function returns a plain string " +
		"(reasonWitReturnsAPlainString in stopSurfaces), so a component guest receives the refusal as an empty string and cannot be " +
		"told to stop at it: it would carry on past the refused call, back into the loop the kill interrupted",
	"freshSetScope": "the host refuses it in a segment (cleat#4091), but its WIT function returns a plain string " +
		"(reasonWitReturnsAPlainString in stopSurfaces), so a component guest cannot be told to stop at it, for the same reason as " +
		"CreatePromise",
	"DurableCompleteUpdate": "no WIT function (reasonNotInTheComponentWorld in stopSurfaces): a component guest cannot call it, so " +
		"it cannot be stopped at it and its event cannot appear in a component's history",
	"AwaitPromise": "an await of a promise only reads, so a cleanup has no effect of it to see; its refusal is a suspension " +
		"(suspendingAwaitResult), not a stop at a first event",
	"WaitForEvent": "waiting for a plugin event only reads, and wait_for_event.go names no event type of its own on its " +
		"fresh path for a stop point to key on",
	"SideEffectRecorded": "the side-effect lookup (cleat#4113) records no event of its own: a lookup that finds nothing changes " +
		"nothing, and one that finds a value consumes the side_effect event the guest's following call would otherwise have recorded, which " +
		"is that call's event and not this one's. It refuses a FRESH lookup in a stopped workflow only so a guest's side-effect function " +
		"never runs, which the stop-site guards check on their own",
}

func TestEveryHostStopSiteIsClassifiedForTheStoppableSet(t *testing.T) {
	sites := hostStopSites(t)
	if len(sites) < 15 {
		t.Fatalf("PRECONDITION FAILED: hostStopSites found %d sites; the scan is reading nothing, so every check below would "+
			"pass vacuously", len(sites))
	}

	var unclassified, stale []string
	claimed := map[stopEvent]bool{}
	for name := range sites {
		ev, listed := stopSiteEvents[name]
		_, exempt := stopSiteNotStoppable[name]
		switch {
		case listed && exempt:
			t.Errorf("host stop site %s is both mapped to an event and exempted", name)
		case listed:
			claimed[ev] = true
			if len(stopSurfaces[name].wit) == 0 {
				t.Errorf("host stop site %s is mapped to %q, but the component world has no typed outcome for it "+
					"(stopSurfaces[%q].wit is empty), so a component guest cannot receive the stop at it. Moving it to "+
					"stopSiteNotStoppable is the fix; keeping it lets the guest carry on past the refused call into its loop", name, ev, name)
			}
			inSet := (ev.op == "" && stoppableFirstEvents[ev.typ]) || (ev.op != "" && stoppableByOp[ev.typ][ev.op])
			if !inSet {
				t.Errorf("host stop site %s refuses the call that records %q (op %q), but that is not a stop point in "+
					"stoppableFirstEvents or stoppableByOp: a killed run whose last effect was that call stops EARLIER than "+
					"it should, and its cleanup does not see that effect", name, ev.typ, ev.op)
			}
		case exempt:
		default:
			unclassified = append(unclassified, name)
		}
	}
	for name := range stopSiteEvents {
		if !sites[name] {
			stale = append(stale, name)
		}
	}
	for name := range stopSiteNotStoppable {
		if !sites[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(unclassified)
	sort.Strings(stale)
	if len(unclassified) > 0 {
		t.Errorf("host stop site(s) %s are in neither stopSiteEvents nor stopSiteNotStoppable. Name the first event type each "+
			"records and add it to stoppableFirstEvents, or say in stopSiteNotStoppable why it cannot be a stop point",
			strings.Join(unclassified, ", "))
	}
	if len(stale) > 0 {
		t.Errorf("entries for %s name no host stop site any more; a stale entry excuses something that is not there",
			strings.Join(stale, ", "))
	}

	// The other direction, and the unsafe one: an event in the set that no refusing site claims is a stop point the host may
	// not be able to refuse.
	var stray []string
	for ev := range stoppableFirstEvents {
		if !claimed[stopEvent{typ: ev}] {
			stray = append(stray, string(ev))
		}
	}
	for ev, ops := range stoppableByOp {
		for op := range ops {
			if !claimed[stopEvent{typ: ev, op: op}] {
				stray = append(stray, string(ev)+"/"+op)
			}
		}
	}
	sort.Strings(stray)
	if len(stray) > 0 {
		t.Errorf("stoppableFirstEvents holds %s, which no host stop site claims. A stop point at an event the host cannot refuse "+
			"lets the guest's next call run real work inside the defer segment", strings.Join(stray, ", "))
	}
}
