package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cleat-team/cleat/internal/host"
)

// debugFlags holds parsed command-line flags for the debug subcommand.
type debugFlags struct {
	workflowID string
	entryPoint string
	watch      bool
}

// runDebug is the entry point for the cleatctl debug command.
func runDebug(ctx context.Context, store host.WorkflowStore, db *sql.DB, args []string) {
	flags := parseDebugFlags(args)
	if flags == nil {
		return // usage already printed
	}

	if flags.watch {
		if err := runDebugWatch(ctx, store, flags.workflowID); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			osExit(1)
		}
		return
		}

	runDebugStep(ctx, store, db, flags.workflowID, flags.entryPoint)
}

// parseDebugFlags parses flags from the args slice. Returns nil if flag
// parsing failed (usage was printed).
func parseDebugFlags(args []string) *debugFlags {
	f := &debugFlags{}

	var positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--entry-point":
			i++
			if i >= len(args) {
				fmt.Fprintln(os.Stderr, "error: --entry-point requires a value")
				return nil
			}
			f.entryPoint = args[i]
		case "--watch":
			f.watch = true
		default:
			positional = append(positional, arg)
		}
	}

	if len(positional) < 1 {
		printDebugUsage()
		return nil
	}
	f.workflowID = positional[0]

	if !f.watch && f.entryPoint == "" {
		fmt.Fprintln(os.Stderr, "error: --entry-point is required for step-through mode")
		fmt.Fprintln(os.Stderr)
		printDebugUsage()
		return nil
	}

	return f
}

// printDebugUsage prints the help text for the debug command.
func printDebugUsage() {
	fmt.Fprintf(os.Stderr, `Usage: cleatctl [--db <dsn>] debug <workflow-id> --entry-point <name> [--watch]

Step-through or watch a workflow's event history for debugging.

Arguments:
  <workflow-id>         The workflow instance ID to debug

Flags:
  --entry-point <name>  WASM export name to invoke (required for step-through,
                        optional with --watch)
  --watch               Watch mode: tail new events as they arrive

Interactive commands (step-through mode):
  next (n)              Advance one event and display step info
  continue (c)          Run remaining events without pausing (final results only)
  state (s)             Dump full query_state key-value map
  events (e)            List remaining event types with indices
  help (h)              Show available commands
  quit (q)              Exit debugger cleanly

`)
}

// debugState holds the state for an interactive debug session.
type debugState struct {
	events       []host.EventRecord
	reader       *bufio.Reader
	autoContinue bool

	stepCh chan debugStepInfo
	cmdCh  chan host.ReplayStepAction
	quit   chan struct{}
	doneCh chan error

	lastStep  int
	lastEvent *host.EventRecord
	lastQS    map[string]string

	replayErr error // captured replay error from interactiveLoop doneCh
}

// debugStepInfo carries step information from the callback to the display loop.
type debugStepInfo struct {
	step  int
	event *host.EventRecord
	qs    map[string]string
}

// runDebugStep runs the interactive step-through debugger for a workflow.
func runDebugStep(ctx context.Context, store host.WorkflowStore, db *sql.DB, workflowID, entryPoint string) {
	inst, err := loadWorkflowInstance(ctx, db, workflowID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading workflow instance %q: %v\n", workflowID, err)
		osExit(1)
	}

	fmt.Printf("Workflow: %s\n", workflowID)
	fmt.Printf("  Definition: %s (v%d)\n", inst.DefName, inst.DefVersion)
	fmt.Printf("  Status: %s\n", inst.Status)
	fmt.Printf("  Entry point: %s\n", entryPoint)

	events, err := store.LoadEventHistory(ctx, workflowID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading event history: %v\n", err)
		osExit(1)
	}
	fmt.Printf("  Events: %d\n", len(events))

	if len(events) == 0 {
		fmt.Println("No events in history. Nothing to debug.")
		return
	}

	wasmBytes, err := store.LoadWASM(ctx, inst.DefName, inst.DefVersion)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading WASM for %s v%d: %v\n", inst.DefName, inst.DefVersion, err)
		osExit(1)
	}
	fmt.Printf("  WASM size: %d bytes\n", len(wasmBytes))

	rt, err := host.NewRuntime(ctx, 0, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating runtime: %v\n", err)
		osExit(1)
	}

	ds := &debugState{
		events: events,
		reader: bufio.NewReader(os.Stdin),
		stepCh: make(chan debugStepInfo),
		cmdCh:  make(chan host.ReplayStepAction),
		quit:   make(chan struct{}),
		doneCh: make(chan error, 1),
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT)
	go func() {
		select {
		case <-sigCh:
			close(ds.quit)
		case <-ctx.Done():
		}
	}()

	engine := host.NewEngine(rt, &replayStubCaller{},
		host.WithDefName(inst.DefName),
		host.WithDefVersion(inst.DefVersion),
		host.WithReplayStepCallback(ds.callback),
	)

	fmt.Println()
	fmt.Println("Starting step-through replay...")
	fmt.Println("Commands: next (n), continue (c), state (s), events (e), help (h), quit (q)")
	fmt.Println()

	replayCtx, replayCancel := context.WithCancel(ctx)
	defer replayCancel()
	go func() {
		_, _, _, _, _, err := engine.Replay(replayCtx, wasmBytes, entryPoint, inst.Input, events)
		ds.doneCh <- err
	}()

	ds.interactiveLoop()

	replayCancel()
	engineErr := ds.replayErr
	if engineErr == nil {
		engineErr = <-ds.doneCh
	}

	fmt.Println()
	if engineErr != nil {
		if engineErr == context.Canceled {
			fmt.Println("Debug session ended.")
		} else {
			fmt.Fprintf(os.Stderr, "replay error: %v\n", engineErr)
		}
	} else {
		fmt.Println("Replay complete.")
	}
}

// callback is the ReplayStepCallback invoked by the engine after each event.
func (d *debugState) callback(step int, event *host.EventRecord, qs map[string]string) host.ReplayStepAction {
	if d.autoContinue {
		d.lastStep = step
		d.lastEvent = event
		d.lastQS = qs
		return host.ReplayNext
	}

	d.stepCh <- debugStepInfo{step: step, event: event, qs: qs}

	select {
	case action := <-d.cmdCh:
		return action
	case <-d.quit:
		return host.ReplayQuit
	}
}

// interactiveLoop reads stdin commands and drives the step-through display.
func (d *debugState) interactiveLoop() {
	for {
		select {
		case info := <-d.stepCh:
			d.displayStep(info)
			d.readCommand()
		case <-d.quit:
			return
		case err := <-d.doneCh:
			d.replayErr = err
			return
		}
	}
}

// displayStep prints the current step information.
func (d *debugState) displayStep(info debugStepInfo) {
	total := len(d.events)
	stepNum := info.step + 1

	fmt.Printf("── Step %d/%d", stepNum, total)

	ev := info.event
	if ev != nil {
		fmt.Printf(" ── type=%s", ev.EventType)
		switch {
		case ev.Service != "" || ev.Op != "":
			fmt.Printf(" ── service=%s ── op=%s", ev.Service, ev.Op)
		case ev.SignalName != "":
			fmt.Printf(" ── signal=%s", ev.SignalName)
		case ev.StateOp != "":
			fmt.Printf(" ── state_op=%s ── key=%s", ev.StateOp, ev.StateKey)
		case ev.DetachedName != "":
			fmt.Printf(" ── detached=%s", ev.DetachedName)
		case ev.FetchURL != "":
			fmt.Printf(" ── fetch=%s %s", ev.FetchMethod, ev.FetchURL)
		case ev.PromiseID != "":
			fmt.Printf(" ── promise=%s", ev.PromiseID)
		}
	}
	fmt.Println()

	if ev != nil {
		if ev.Request != "" {
			fmt.Printf("  request:   %s\n", ev.Request)
		}
		if ev.Response != "" {
			fmt.Printf("  response:  %s\n", ev.Response)
		}
		if ev.Err != "" {
			fmt.Printf("  error:     %s\n", ev.Err)
		}
		if ev.SignalPayload != "" {
			fmt.Printf("  payload:   %s\n", ev.SignalPayload)
		}
	}

	fmt.Printf("  query_state: %s\n", formatQueryState(info.qs))
}

// readCommand reads a command from stdin and dispatches it.
func (d *debugState) readCommand() {
	for {
		fmt.Printf("debug> ")
		line, err := d.reader.ReadString('\n')
		if err != nil {
			d.sendAction(host.ReplayQuit)
			return
		}
		cmd := strings.TrimSpace(line)

		switch cmd {
		case "next", "n", "":
			d.sendAction(host.ReplayNext)
			return

		case "continue", "c":
			d.autoContinue = true
			d.sendAction(host.ReplayNext)
			return

		case "state", "s":
			if d.lastQS != nil {
				fmt.Println("query_state:")
				for k, v := range d.lastQS {
					fmt.Printf("  %s = %s\n", k, v)
				}
			} else if d.lastEvent != nil {
				// Fall back to last displayed step
				fmt.Println("query_state: {}")
			} else {
				fmt.Println("(no query state yet — advance at least one step)")
			}

		case "events", "e":
			fmt.Print(formatRemainingEvents(d.events, d.lastStep+1))

		case "help", "h":
			fmt.Println()
			fmt.Println("Commands:")
			fmt.Println("  next (n) / Enter   Advance one event")
			fmt.Println("  continue (c)       Run remaining events without pausing")
			fmt.Println("  state (s)          Dump full query_state")
			fmt.Println("  events (e)         List remaining event types")
			fmt.Println("  help (h)           Show this help")
			fmt.Println("  quit (q)           Exit debugger")
			fmt.Println()

		case "quit", "q":
			d.sendAction(host.ReplayQuit)
			return

		default:
			fmt.Printf("unknown command: %q (type 'help' for available commands)\n", cmd)
		}
	}
}

// sendAction sends an action to the callback goroutine, or handles quit.
func (d *debugState) sendAction(action host.ReplayStepAction) {
	select {
	case d.cmdCh <- action:
	case <-d.quit:
	}
}

// runDebugWatch runs the live event tailing watch mode.
func runDebugWatch(ctx context.Context, store host.WorkflowStore, workflowID string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, os.Interrupt)
	defer signal.Stop(sigCh)
	go func() {
		<-sigCh
		cancel()
	}()

	initialCount, err := store.CountEventHistory(ctx, workflowID)
	if err != nil {
		return fmt.Errorf("error counting events for %q: %w", workflowID, err)
	}

	fmt.Printf("Watching workflow %s (%d events so far)...\n", workflowID, initialCount)
	fmt.Println("(Ctrl+C to stop)")

	lastSeen := initialCount
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	idleStart := time.Time{}

	for {
		select {
		case <-ctx.Done():
			fmt.Println()
			fmt.Println("Watch ended.")
			return nil
		case <-ticker.C:
			currentCount, err := store.CountEventHistory(ctx, workflowID)
			if err != nil {
				if ctx.Err() != nil {
					fmt.Println()
					fmt.Println("Watch ended.")
					return nil
				}
				fmt.Fprintf(os.Stderr, "error polling events: %v\n", err)
				continue
			}

			if currentCount > lastSeen {
				newEvents, err := store.LoadEventHistoryPaginated(ctx, workflowID, lastSeen, 100)
				if err != nil {
					fmt.Fprintf(os.Stderr, "error loading new events: %v\n", err)
					continue
				}
				for _, ev := range newEvents {
					fmt.Printf("  [%d] %s\n", ev.Step, formatEvent(ev))
				}
				lastSeen = currentCount
				idleStart = time.Time{}
			} else if lastSeen > 0 {
				if idleStart.IsZero() {
					idleStart = time.Now()
				} else if time.Since(idleStart) > 60*time.Second {
					fmt.Println("No new events for 60s — exiting watch mode.")
					return nil
				}
			}
		}
	}
}

// formatQueryState returns a compact JSON representation of the query state.
func formatQueryState(qs map[string]string) string {
	if len(qs) == 0 {
		return "{}"
	}
	b, _ := json.Marshal(qs)
	return string(b)
}

// formatRemainingEvents returns a formatted list of remaining events.
func formatRemainingEvents(events []host.EventRecord, fromStep int) string {
	if fromStep >= len(events) {
		return "(no remaining events)\n"
	}
	var sb strings.Builder
	sb.WriteString("Remaining events:\n")
	for i := fromStep; i < len(events); i++ {
		ev := events[i]
		fmt.Fprintf(&sb, "  [%d] step=%d type=%s", i-fromStep+1, ev.Step, ev.EventType)
		if ev.Service != "" {
			fmt.Fprintf(&sb, " service=%s op=%s", ev.Service, ev.Op)
		}
		if ev.SignalName != "" {
			fmt.Fprintf(&sb, " signal=%s", ev.SignalName)
		}
		if ev.StateOp != "" {
			fmt.Fprintf(&sb, " state_op=%s key=%s", ev.StateOp, ev.StateKey)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// formatEvent returns a one-line summary of an EventRecord.
func formatEvent(ev host.EventRecord) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "type=%s", ev.EventType)
	if ev.Service != "" {
		fmt.Fprintf(&sb, " service=%s op=%s", ev.Service, ev.Op)
	}
	if ev.SignalName != "" {
		fmt.Fprintf(&sb, " signal=%s", ev.SignalName)
	}
	if ev.Request != "" {
		fmt.Fprintf(&sb, " request=%s", truncate(ev.Request, 60))
	}
	if ev.Response != "" {
		fmt.Fprintf(&sb, " response=%s", truncate(ev.Response, 60))
	}
	if ev.Err != "" {
		fmt.Fprintf(&sb, " err=%s", truncate(ev.Err, 60))
	}
	return sb.String()
}
