package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"

	"github.com/cleat-team/cleat/plugins/clewservice"
)

func main() {
	addr := flag.String("addr", ":8081", "HTTP listen address")
	projectRoot := flag.String("project-root", "", "Path to clew-dogfood repo (required)")
	flag.Parse()

	if *projectRoot == "" {
		*projectRoot = os.Getenv("CLEW_PROJECT_ROOT")
	}
	if *projectRoot == "" {
		slog.Error("project-root is required (via --project-root or CLEW_PROJECT_ROOT)")
		os.Exit(1)
	}

	p := &clewservice.Plugin{}
	if err := p.InitStandalone(*projectRoot); err != nil {
		slog.Error("init failed", "error", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		slog.Error("register routes failed", "error", err)
		os.Exit(1)
	}

	slog.Info("clew-service starting", "addr", *addr, "project_root", *projectRoot)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		slog.Error("server failed", "error", err)
		os.Exit(1)
	}
}
