// Command idempotent-start shows how to start a workflow so the start is safe
// to retry, and how to wait for its result.
//
// THE PROBLEM THIS SOLVES. A client sends POST /start and gets no reply. Three
// things could have happened and it cannot tell them apart:
//
//	a) the server never received the request
//	b) the server received it and is still processing
//	c) the server finished and the reply was lost
//
// Retrying blindly without a key risks starting the work twice in cases (b) and
// (c). Not retrying risks never starting it at all in case (a).
//
// With an idempotency key the client does not have to tell them apart: it
// retries with the SAME key and the server's answer is the disambiguation. In
// (a) the retry starts the run; in (b) it waits on the key and returns the
// winner; in (c) it returns the winner. Exactly one run exists either way,
// because the server writes the key and the workflow row in one transaction.
//
// See docs/reference/workflow-lifecycle.md, "Writing a client that retries a
// start", for the full contract.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/cleat-team/cleat/cleat/backendkit"
)

func main() {
	c := backendkit.New(envOr("CLEAT_API", "http://localhost:8080"))

	// ONE key per logical request, reused across every retry OF THAT REQUEST.
	// A fresh key per attempt would defeat the whole mechanism; a shared key
	// across different requests makes the second one a 409 mismatch.
	//
	// It comes from the caller's own domain -- an order id, a request id --
	// because it has to survive the process that generated it crashing and
	// being restarted. A UUID minted here would not.
	key := envOr("ORDER_ID", "order-12345")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	res, err := startWithRetry(ctx, c, key, json.RawMessage(`{"amount":1000}`))
	if err != nil {
		log.Fatalf("start: %v", err)
	}

	// Worth logging, not worth branching the happy path on: false means this
	// call created the run, true means an earlier attempt did.
	log.Printf("run %s (idempotent_replay=%v)", res.ID, res.IdempotentReplay)

	result, err := waitForResult(ctx, c, res.ID)
	if err != nil {
		log.Fatalf("wait: %v", err)
	}
	fmt.Println(result)
}

// startWithRetry is LOOP ONE: "did my request land?"
//
// Bounded, because a failure here means something is wrong. Only transport
// failures are retried -- the two idempotency refusals are permanent, and
// repeating a request the server has already rejected as different changes
// nothing.
func startWithRetry(ctx context.Context, c *backendkit.Client, key string, input json.RawMessage) (backendkit.StartResult, error) {
	var last error
	for attempt := 0; attempt < 5; attempt++ {
		res, err := c.StartWorkflowWithOptions(ctx, "charge", input,
			backendkit.StartOptions{IdempotencyKey: key})
		if err == nil {
			return res, nil
		}
		// PERMANENT. The key was used before with a different payload or a
		// different definition, so the caller sent two different requests under
		// one key and has to decide which it meant.
		if errors.Is(err, backendkit.ErrIdempotencyKeyInputMismatch) ||
			errors.Is(err, backendkit.ErrIdempotencyKeyDefinitionMismatch) {
			return backendkit.StartResult{}, err
		}
		last = err
		select {
		case <-ctx.Done():
			return backendkit.StartResult{}, ctx.Err()
		case <-time.After(time.Duration(1<<attempt) * 200 * time.Millisecond):
		}
	}
	return backendkit.StartResult{}, fmt.Errorf("start did not land after 5 attempts: %w", last)
}

// waitForResult is LOOP TWO: "is the work finished?"
//
// SEPARATE FROM LOOP ONE, and that is the point rather than an accident. This
// one is unbounded in a way the first must not be: a durable workflow may
// legitimately sleep for days. Sharing one retry budget between them makes a
// bounded "did it land" budget govern an unbounded wait, and "gave up" then
// looks exactly like "the workflow failed".
//
// The result comes from the RUN, never from the start response -- which carries
// no result even when the winner has already finished.
func waitForResult(ctx context.Context, c *backendkit.Client, runID string) (string, error) {
	for {
		wf, err := c.GetWorkflow(ctx, runID)
		if err != nil {
			// A failed read is not a failed workflow. Keep waiting.
			log.Printf("poll: %v", err)
		} else {
			switch wf.Status {
			case "done":
				return wf.Result, nil
			case "failed", "terminated", "dead_lettered":
				return "", fmt.Errorf("workflow %s ended %s: %s", runID, wf.Status, wf.Error)
			}
			// Everything else -- ready, running, terminating -- is still
			// outstanding. Note that a sleeping workflow reports "ready", not
			// "running", so waiting for "running" to disappear would wait
			// forever.
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
