package scheduler

import (
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/rcownie/cleat/internal/plugin"
)

func TestInfo(t *testing.T) {
	p := &Plugin{}
	info := p.Info()
	if info.Name != "scheduler" {
		t.Errorf("expected name 'scheduler', got %q", info.Name)
	}
	if info.Version != "0.1.0" {
		t.Errorf("expected version '0.1.0', got %q", info.Version)
	}
	if info.Description == "" {
		t.Errorf("expected non-empty description")
	}
}

func TestInit(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{
		DB:     &sql.DB{},
		Logger: slog.Default(),
	}
	if err := p.Init(context.Background(), env); err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
	if p.db == nil {
		t.Error("expected db to be set after Init")
	}
	if p.logger == nil {
		t.Error("expected logger to be set after Init")
	}
}

func TestInitWithNilLogger(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{
		DB: &sql.DB{},
	}
	if err := p.Init(context.Background(), env); err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
	if p.logger == nil {
		t.Error("expected logger to be set after Init with nil logger")
	}
}

func TestNextRun_Every5Min(t *testing.T) {
	// */5 * * * *  — every 5 minutes
	base := time.Date(2025, 1, 15, 10, 3, 0, 0, time.UTC)
	next := nextRun("*/5 * * * *", base)
	expected := time.Date(2025, 1, 15, 10, 5, 0, 0, time.UTC)
	if !next.Equal(expected) {
		t.Errorf("*/5: expected %v, got %v", expected, next)
	}
}

func TestNextRun_Hourly(t *testing.T) {
	// 0 * * * *  — at the top of each hour
	base := time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC)
	next := nextRun("0 * * * *", base)
	expected := time.Date(2025, 1, 15, 11, 0, 0, 0, time.UTC)
	if !next.Equal(expected) {
		t.Errorf("hourly: expected %v, got %v", expected, next)
	}
}

func TestNextRun_DailyAt9am(t *testing.T) {
	// 0 9 * * *  — daily at 9am
	base := time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC)
	next := nextRun("0 9 * * *", base)
	expected := time.Date(2025, 1, 16, 9, 0, 0, 0, time.UTC)
	if !next.Equal(expected) {
		t.Errorf("daily 9am: expected %v, got %v", expected, next)
	}
}

func TestNextRun_WeekdaysAt9am(t *testing.T) {
	// 0 9 * * 1-5  — weekdays at 9am
	// Jan 15, 2025 is a Wednesday
	base := time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC)
	next := nextRun("0 9 * * 1-5", base)
	expected := time.Date(2025, 1, 16, 9, 0, 0, 0, time.UTC) // Thursday
	if !next.Equal(expected) {
		t.Errorf("weekdays 9am: expected %v, got %v", expected, next)
	}
}

func TestNextRun_ExactMinute(t *testing.T) {
	// 30 10 * * *  — at 10:30 every day
	base := time.Date(2025, 1, 15, 10, 29, 0, 0, time.UTC)
	next := nextRun("30 10 * * *", base)
	expected := time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC)
	if !next.Equal(expected) {
		t.Errorf("exact minute: expected %v, got %v", expected, next)
	}
}

func TestNextRun_InvalidCron(t *testing.T) {
	// Invalid cron expression should return zero time.
	next := nextRun("invalid", time.Now())
	if !next.IsZero() {
		t.Errorf("expected zero time for invalid cron, got %v", next)
	}
}

func TestNextRun_CommaList(t *testing.T) {
	// 15,30,45 * * * *  — at minutes 15, 30, 45
	base := time.Date(2025, 1, 15, 10, 20, 0, 0, time.UTC)
	next := nextRun("15,30,45 * * * *", base)
	expected := time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC)
	if !next.Equal(expected) {
		t.Errorf("comma list: expected %v, got %v", expected, next)
	}
}

func TestNextRun_Range(t *testing.T) {
	// 0 9-17 * * *  — every hour from 9am to 5pm
	base := time.Date(2025, 1, 15, 8, 0, 0, 0, time.UTC)
	next := nextRun("0 9-17 * * *", base)
	expected := time.Date(2025, 1, 15, 9, 0, 0, 0, time.UTC)
	if !next.Equal(expected) {
		t.Errorf("range: expected %v, got %v", expected, next)
	}
}

func TestPluginRegistration(t *testing.T) {
	plugins, err := plugin.Discover()
	if err != nil {
		t.Fatalf("Discover() returned error: %v", err)
	}
	found := false
	for _, lp := range plugins {
		if lp.Plugin.Info().Name == "scheduler" {
			found = true
			break
		}
	}
	if !found {
		t.Error("scheduler plugin not found after Discover")
	}
}

func TestInitWithConfig(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{
		DB:     &sql.DB{},
		Logger: slog.Default(),
		Config: []byte(`{}`),
	}
	if err := p.Init(context.Background(), env); err != nil {
		t.Fatalf("Init() with config returned error: %v", err)
	}
	if p.db == nil {
		t.Error("expected db to be set after Init")
	}
	if p.logger == nil {
		t.Error("expected logger to be set after Init")
	}
}

func TestInitInvalidConfig(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{
		DB:     &sql.DB{},
		Logger: slog.Default(),
		Config: []byte(`not valid json`),
	}
	err := p.Init(context.Background(), env)
	if err == nil {
		t.Fatal("expected error for invalid config, got nil")
	}
	if !strings.Contains(err.Error(), "config") {
		t.Errorf("expected error mentioning config, got: %v", err)
	}
}

