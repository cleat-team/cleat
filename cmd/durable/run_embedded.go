package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/rcownie/durable/internal/host"
)

func runEmbedded(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	wasmFile := fs.String("wasm", "", "Path to .wasm file (built if not provided)")
	entryPoint := fs.String("entry-point", "place_order", "Entry point function name")
	inputJSON := fs.String("input", "{}", "Workflow input as JSON")
	apiAddr := fs.String("api-addr", ":8080", "HTTP API + web UI listen address (empty to disable)")
	target := fs.String("target", "go", "Build target: go, tinygo, or rust")
	fs.Parse(args)

	remainder := fs.Args()
	if len(remainder) == 0 && *wasmFile == "" {
		fmt.Fprintf(os.Stderr, "Usage: durable run [flags] <package-path>\n")
		fmt.Fprintf(os.Stderr, "       durable run --wasm <file.wasm> [flags]\n")
		fmt.Fprintf(os.Stderr, "\nFlags:\n")
		fs.PrintDefaults()
		os.Exit(1)
	}

	var wasmBytes []byte
	var wfName string

	if *wasmFile != "" {
		// Load pre-built WASM.
		var err error
		wasmBytes, err = os.ReadFile(*wasmFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading WASM file: %v\n", err)
			os.Exit(1)
		}
		wfName = strings.TrimSuffix(filepath.Base(*wasmFile), ".wasm")
	} else {
		// Build then load.
		pkgPath := remainder[0]
		outDir, err := os.MkdirTemp("", "durable-run-*")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error creating temp dir: %v\n", err)
			os.Exit(1)
		}
		defer os.RemoveAll(outDir)

		// Build the workflow.
		runBuild(pkgPath, outDir, *target)

		// Find the .wasm file.
		entries, _ := os.ReadDir(outDir)
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".wasm") {
				wasmBytes, err = os.ReadFile(filepath.Join(outDir, e.Name()))
				if err != nil {
					fmt.Fprintf(os.Stderr, "Error reading built WASM: %v\n", err)
					os.Exit(1)
				}
				wfName = strings.TrimSuffix(e.Name(), ".wasm")
				break
			}
		}
		if wasmBytes == nil {
			fmt.Fprintf(os.Stderr, "Error: no .wasm file produced in %s\n", outDir)
			os.Exit(1)
		}
	}

	// ---- Create in-process runtime ----
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rt, rtErr := host.NewRuntime(ctx)
	if rtErr != nil {
		fmt.Fprintf(os.Stderr, "Error creating runtime: %v\n", rtErr)
		os.Exit(1)
	}
	defer rt.Close(ctx)

	// Use a mock caller that logs and returns placeholder responses.
	caller := &logCaller{}
	engine := host.NewEngine(rt, caller)

	// ---- Run the workflow ----
	fmt.Printf("Running %s.%s with input: %s\n", wfName, *entryPoint, *inputJSON)

	result, history, suspended, deferrals, queryState, execErr := engine.Execute(
		ctx, wasmBytes, *entryPoint, json.RawMessage(*inputJSON),
	)
	if execErr != nil {
		fmt.Fprintf(os.Stderr, "Execution error: %v\n", execErr)
		// Still show history.
		fmt.Printf("\nEvent history (%d events):\n", len(history))
		for _, rec := range history {
			fmt.Printf("  [%d] %s", rec.Step, rec.EventType)
			if rec.Service != "" {
				fmt.Printf(" %s.%s", rec.Service, rec.Op)
			}
			fmt.Println()
		}
		os.Exit(1)
	}

	if suspended != nil {
		fmt.Printf("\nWorkflow suspended: %s (until %s)\n", suspended.Reason, suspended.SuspendUntil)
		fmt.Printf("Deferrals: %d\n", len(deferrals))
		// Print event history.
		fmt.Printf("\nEvent history (%d events):\n", len(suspended.History))
		for _, rec := range suspended.History {
			fmt.Printf("  [%d] %s", rec.Step, rec.EventType)
			if rec.Service != "" {
				fmt.Printf(" %s.%s", rec.Service, rec.Op)
			}
			if rec.Err != "" {
				fmt.Printf(" ERROR: %s", rec.Err)
			}
			fmt.Println()
		}
	} else {
		fmt.Printf("\nResult: %s\n", result)
		fmt.Printf("Query state: %v\n", queryState)
		fmt.Printf("Deferrals: %d\n", len(deferrals))
		fmt.Printf("History: %d events\n", len(history))
	}

	// ---- Optionally serve HTTP API + web UI ----
	if *apiAddr != "" {
		fmt.Printf("\nServing inspection API at http://localhost%s\n", *apiAddr)
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"ok":true}`))
		})
		mux.HandleFunc("/history", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			h := history
			if suspended != nil {
				h = suspended.History
			}
			json.NewEncoder(w).Encode(h)
		})
		mux.HandleFunc("/result", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"result":     result,
				"suspended":  suspended,
				"deferrals":  deferrals,
				"queryState": queryState,
				"historyLen": len(history),
			})
		})

		srv := &http.Server{Addr: *apiAddr, Handler: mux}
		go func() {
			if err := srv.ListenAndServe(); err != http.ErrServerClosed {
				log.Printf("HTTP server: %v", err)
			}
		}()

		// Wait for Ctrl+C.
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		fmt.Println("Press Ctrl+C to exit.")
		<-sigCh
		srv.Shutdown(context.Background())
	}
}

// logCaller is a mock ServiceCaller that logs calls and returns placeholder responses.
type logCaller struct{}

func (c *logCaller) Call(ctx context.Context, service, operation, requestJSON string) (string, error) {
	log.Printf("[durable run] %s.%s(%s)", service, operation, truncate(requestJSON, 100))
	// Return a placeholder success response so workflows can run end-to-end.
	resp := map[string]interface{}{
		"ok":      true,
		"service": service,
		"op":      operation,
		"echo":    requestJSON,
	}
	b, _ := json.Marshal(resp)
	return string(b), nil
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
