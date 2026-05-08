package plugin

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestValidateCapabilitiesNoViolations(t *testing.T) {
	declared := CapabilityLimits{
		Database:       true,
		SignalWorkflow: true,
	}
	limits := CapabilityLimits{
		Database:       true,
		SignalWorkflow: true,
	}
	if err := ValidateCapabilities(declared, limits); err != nil {
		t.Errorf("expected no error, got: %v", err)
	}
}

func TestValidateCapabilitiesDatabaseDenied(t *testing.T) {
	declared := CapabilityLimits{
		Database: true,
	}
	limits := CapabilityLimits{
		Database: false,
	}
	err := ValidateCapabilities(declared, limits)
	if err == nil {
		t.Fatal("expected error for database denied")
	}
	if !strings.Contains(err.Error(), "database access denied") {
		t.Errorf("expected 'database access denied', got: %v", err)
	}
}

func TestValidateCapabilitiesStartWorkflowDenied(t *testing.T) {
	declared := CapabilityLimits{
		StartWorkflow: true,
	}
	limits := CapabilityLimits{
		StartWorkflow: false,
	}
	err := ValidateCapabilities(declared, limits)
	if err == nil {
		t.Fatal("expected error for start_workflow denied")
	}
	if !strings.Contains(err.Error(), "start_workflow denied") {
		t.Errorf("expected 'start_workflow denied', got: %v", err)
	}
}

func TestValidateCapabilitiesSignalWorkflowDenied(t *testing.T) {
	declared := CapabilityLimits{
		SignalWorkflow: true,
	}
	limits := CapabilityLimits{
		SignalWorkflow: false,
	}
	err := ValidateCapabilities(declared, limits)
	if err == nil {
		t.Fatal("expected error for signal_workflow denied")
	}
	if !strings.Contains(err.Error(), "signal_workflow denied") {
		t.Errorf("expected 'signal_workflow denied', got: %v", err)
	}
}

func TestValidateCapabilitiesCallPluginDenied(t *testing.T) {
	declared := CapabilityLimits{
		CallPlugin: []string{"foo"},
	}
	limits := CapabilityLimits{
		CallPlugin: []string{"bar"},
	}
	err := ValidateCapabilities(declared, limits)
	if err == nil {
		t.Fatal("expected error for call_plugin denied")
	}
	if !strings.Contains(err.Error(), `call_plugin "foo" denied`) {
		t.Errorf("expected 'call_plugin \"foo\" denied', got: %v", err)
	}
}

func TestValidateCapabilitiesCallPluginWildcard(t *testing.T) {
	declared := CapabilityLimits{
		CallPlugin: []string{"foo", "bar"},
	}
	limits := CapabilityLimits{
		CallPlugin: []string{"*"},
	}
	if err := ValidateCapabilities(declared, limits); err != nil {
		t.Errorf("expected no error with wildcard, got: %v", err)
	}
}

func TestValidateCapabilitiesCallPluginExactMatch(t *testing.T) {
	declared := CapabilityLimits{
		CallPlugin: []string{"foo", "bar"},
	}
	limits := CapabilityLimits{
		CallPlugin: []string{"foo", "bar", "baz"},
	}
	if err := ValidateCapabilities(declared, limits); err != nil {
		t.Errorf("expected no error with exact match, got: %v", err)
	}
}

func TestValidateCapabilitiesMultipleViolations(t *testing.T) {
	declared := CapabilityLimits{
		Database:       true,
		StartWorkflow:  true,
		SignalWorkflow: true,
		HTTPRoutes:     true,
		CallPlugin:     []string{"foo"},
	}
	limits := CapabilityLimits{
		Database:         false,
		StartWorkflow:    false,
		SignalWorkflow:   false,
		HTTPRoutes:       false,
		HTTPMiddleware:   false,
		BackgroundWorker: false,
	}
	err := ValidateCapabilities(declared, limits)
	if err == nil {
		t.Fatal("expected error for multiple violations")
	}
	msg := err.Error()
	if !strings.Contains(msg, "database access denied") {
		t.Errorf("expected 'database access denied' in error")
	}
	if !strings.Contains(msg, "start_workflow denied") {
		t.Errorf("expected 'start_workflow denied' in error")
	}
	if !strings.Contains(msg, "signal_workflow denied") {
		t.Errorf("expected 'signal_workflow denied' in error")
	}
	if !strings.Contains(msg, "http_routes denied") {
		t.Errorf("expected 'http_routes denied' in error")
	}
	if !strings.Contains(msg, `call_plugin "foo" denied`) {
		t.Errorf("expected 'call_plugin \"foo\" denied' in error")
	}
}

func TestDefaultLimits(t *testing.T) {
	limits := DefaultLimits()
	if !limits.Database {
		t.Error("expected Database to be true")
	}
	if limits.StartWorkflow {
		t.Error("expected StartWorkflow to be false")
	}
	if !limits.SignalWorkflow {
		t.Error("expected SignalWorkflow to be true")
	}
	if limits.HTTPRoutes {
		t.Error("expected HTTPRoutes to be false")
	}
	if limits.HTTPMiddleware {
		t.Error("expected HTTPMiddleware to be false")
	}
	if limits.BackgroundWorker {
		t.Error("expected BackgroundWorker to be false")
	}
	if limits.CallPlugin != nil {
		t.Error("expected CallPlugin to be nil")
	}
}

func TestValidateCapabilitiesEmptyDeclared(t *testing.T) {
	declared := CapabilityLimits{}
	limits := DefaultLimits()
	if err := ValidateCapabilities(declared, limits); err != nil {
		t.Errorf("expected no error for empty declared, got: %v", err)
	}
}

func TestValidateCapabilitiesHTTPRoutesDenied(t *testing.T) {
	declared := CapabilityLimits{
		HTTPRoutes: true,
	}
	limits := CapabilityLimits{}
	err := ValidateCapabilities(declared, limits)
	if err == nil {
		t.Fatal("expected error for http_routes denied")
	}
	if !strings.Contains(err.Error(), "http_routes denied") {
		t.Errorf("expected 'http_routes denied', got: %v", err)
	}
}

func TestValidateCapabilitiesHTTPMiddlewareDenied(t *testing.T) {
	declared := CapabilityLimits{
		HTTPMiddleware: true,
	}
	limits := CapabilityLimits{}
	err := ValidateCapabilities(declared, limits)
	if err == nil {
		t.Fatal("expected error for http_middleware denied")
	}
	if !strings.Contains(err.Error(), "http_middleware denied") {
		t.Errorf("expected 'http_middleware denied', got: %v", err)
	}
}

func TestValidateCapabilitiesBackgroundWorkerDenied(t *testing.T) {
	declared := CapabilityLimits{
		BackgroundWorker: true,
	}
	limits := CapabilityLimits{}
	err := ValidateCapabilities(declared, limits)
	if err == nil {
		t.Fatal("expected error for background_worker denied")
	}
	if !strings.Contains(err.Error(), "background_worker denied") {
		t.Errorf("expected 'background_worker denied', got: %v", err)
	}
}

func TestDeriveCapabilitiesNoInterfaces(t *testing.T) {
	p := &noopPlugin{}
	caps := DeriveCapabilities(p)
	if !caps.Database {
		t.Error("expected Database to be true for all Go plugins")
	}
	if caps.HTTPRoutes {
		t.Error("expected HTTPRoutes to be false for noop")
	}
	if caps.HTTPMiddleware {
		t.Error("expected HTTPMiddleware to be false for noop")
	}
	if caps.BackgroundWorker {
		t.Error("expected BackgroundWorker to be false for noop")
	}
}

// routesPlugin implements HasRoutes.
type routesPlugin struct {
	noopPlugin
}

func (p *routesPlugin) RegisterRoutes(mux *http.ServeMux) error {
	return nil
}

func TestDeriveCapabilitiesWithRoutes(t *testing.T) {
	p := &routesPlugin{}
	caps := DeriveCapabilities(p)
	if !caps.Database {
		t.Error("expected Database to be true")
	}
	if !caps.HTTPRoutes {
		t.Error("expected HTTPRoutes to be true for routesPlugin")
	}
	if caps.HTTPMiddleware {
		t.Error("expected HTTPMiddleware to be false")
	}
	if caps.BackgroundWorker {
		t.Error("expected BackgroundWorker to be false")
	}
}

// middlewarePlugin implements HasMiddleware.
type middlewarePlugin struct {
	noopPlugin
}

func (p *middlewarePlugin) Middleware(next http.Handler) http.Handler {
	return next
}

func TestDeriveCapabilitiesWithMiddleware(t *testing.T) {
	p := &middlewarePlugin{}
	caps := DeriveCapabilities(p)
	if !caps.Database {
		t.Error("expected Database to be true")
	}
	if caps.HTTPRoutes {
		t.Error("expected HTTPRoutes to be false")
	}
	if !caps.HTTPMiddleware {
		t.Error("expected HTTPMiddleware to be true for middlewarePlugin")
	}
	if caps.BackgroundWorker {
		t.Error("expected BackgroundWorker to be false")
	}
}

// backgroundPlugin implements HasBackground.
type backgroundPlugin struct {
	noopPlugin
}

func (p *backgroundPlugin) Run(ctx context.Context) error {
	return nil
}

func TestDeriveCapabilitiesWithBackground(t *testing.T) {
	p := &backgroundPlugin{}
	caps := DeriveCapabilities(p)
	if !caps.Database {
		t.Error("expected Database to be true")
	}
	if caps.HTTPRoutes {
		t.Error("expected HTTPRoutes to be false")
	}
	if caps.HTTPMiddleware {
		t.Error("expected HTTPMiddleware to be false")
	}
	if !caps.BackgroundWorker {
		t.Error("expected BackgroundWorker to be true for backgroundPlugin")
	}
}

func TestCapabilityLimitsIsSet(t *testing.T) {
	var empty CapabilityLimits
	if empty.IsSet() {
		t.Error("expected IsSet() to be false for zero-value CapabilityLimits")
	}

	if !DefaultLimits().IsSet() {
		t.Error("expected DefaultLimits().IsSet() to be true")
	}

	if !(CapabilityLimits{Database: true}).IsSet() {
		t.Error("expected IsSet() to be true when Database is true")
	}

	if !(CapabilityLimits{CallPlugin: []string{"foo"}}).IsSet() {
		t.Error("expected IsSet() to be true when CallPlugin is set")
	}
}
