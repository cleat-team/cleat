package main

import (
	"math"
	"strings"
	"testing"
)

// approxEqual compares two float64 values within a small epsilon (0.005).
func approxEqual(a, b float64) bool {
	return math.Abs(a-b) < 0.005
}

func TestAWSDBInstance(t *testing.T) {
	tests := []struct {
		workload int
		want     string
	}{
		{0, "db.r6g.large"},
		{500, "db.r6g.large"},
		{1000, "db.r6g.large"},
		{1001, "db.r6g.xlarge"},
		{5000, "db.r6g.xlarge"},
		{10000, "db.r6g.xlarge"},
		{10001, "db.r6g.2xlarge"},
		{25000, "db.r6g.2xlarge"},
		{50000, "db.r6g.2xlarge"},
		{50001, "db.r6g.4xlarge"},
		{100000, "db.r6g.4xlarge"},
		{200000, "db.r6g.4xlarge"},
		{200001, "db.r6g.8xlarge"},
		{500000, "db.r6g.8xlarge"},
	}
	for _, tt := range tests {
		c := &costCommand{workload: tt.workload}
		got := c.awsDBInstance()
		if got != tt.want {
			t.Errorf("awsDBInstance(workload=%d) = %q, want %q", tt.workload, got, tt.want)
		}
	}
}

func TestGCPDBInstance(t *testing.T) {
	tests := []struct {
		workload int
		want     string
	}{
		{0, "db-custom-2-8"},
		{500, "db-custom-2-8"},
		{1000, "db-custom-2-8"},
		{1001, "db-custom-4-16"},
		{5000, "db-custom-4-16"},
		{10000, "db-custom-4-16"},
		{10001, "db-custom-8-32"},
		{25000, "db-custom-8-32"},
		{50000, "db-custom-8-32"},
		{50001, "db-custom-16-64"},
		{100000, "db-custom-16-64"},
		{200000, "db-custom-16-64"},
		{200001, "db-custom-32-128"},
		{500000, "db-custom-32-128"},
	}
	for _, tt := range tests {
		c := &costCommand{workload: tt.workload}
		got := c.gcpDBInstance()
		if got != tt.want {
			t.Errorf("gcpDBInstance(workload=%d) = %q, want %q", tt.workload, got, tt.want)
		}
	}
}

func TestSelfDBInstance(t *testing.T) {
	c := &costCommand{}
	got := c.selfDBInstance()
	want := "self-hosted (8 vCPU, 32 GB)"
	if got != want {
		t.Errorf("selfDBInstance() = %q, want %q", got, want)
	}

	// Should be the same regardless of workload.
	c.workload = 999999
	got = c.selfDBInstance()
	if got != want {
		t.Errorf("selfDBInstance() with large workload = %q, want %q", got, want)
	}
}

func TestAWSDBCost(t *testing.T) {
	tests := []struct {
		workload int
		want     float64
	}{
		{0, 0.239 * 730},    // db.r6g.large
		{1000, 0.239 * 730},  // db.r6g.large
		{5000, 0.479 * 730},  // db.r6g.xlarge
		{10000, 0.479 * 730}, // db.r6g.xlarge
		{25000, 0.958 * 730}, // db.r6g.2xlarge
		{50000, 0.958 * 730}, // db.r6g.2xlarge
		{100000, 1.916 * 730}, // db.r6g.4xlarge
		{200000, 1.916 * 730}, // db.r6g.4xlarge
		{500000, 3.832 * 730}, // db.r6g.8xlarge
	}
	for _, tt := range tests {
		c := &costCommand{workload: tt.workload}
		got := c.awsDBCost()
		if !approxEqual(got, tt.want) {
			t.Errorf("awsDBCost(workload=%d) = %v, want %v (delta)", tt.workload, got, tt.want)
		}
	}
}

func TestGCPDBCost(t *testing.T) {
	tests := []struct {
		workload int
		want     float64
	}{
		{0, (2*0.047 + 8*0.006) * 730},   // db-custom-2-8
		{1000, (2*0.047 + 8*0.006) * 730}, // db-custom-2-8
		{5000, (4*0.047 + 16*0.006) * 730}, // db-custom-4-16
		{25000, (8*0.047 + 32*0.006) * 730}, // db-custom-8-32
		{100000, (16*0.047 + 64*0.006) * 730}, // db-custom-16-64
		{500000, (32*0.047 + 128*0.006) * 730}, // db-custom-32-128
	}
	for _, tt := range tests {
		c := &costCommand{workload: tt.workload}
		got := c.gcpDBCost()
		if !approxEqual(got, tt.want) {
			t.Errorf("gcpDBCost(workload=%d) = %v, want %v (delta)", tt.workload, got, tt.want)
		}
	}
}

func TestSelfDBCost(t *testing.T) {
	c := &costCommand{}
	got := c.selfDBCost()
	want := 400.0
	if got != want {
		t.Errorf("selfDBCost() = %v, want %v", got, want)
	}

	// Should be fixed regardless of workload.
	c.workload = 999999
	got = c.selfDBCost()
	if got != want {
		t.Errorf("selfDBCost() with large workload = %v, want %v", got, want)
	}
}

func TestAWSStorageCost(t *testing.T) {
	c := &costCommand{}
	tests := []struct {
		totalGB float64
		want    float64
	}{
		{0, 0},
		{100, 100 * 0.115},
		{500, 500 * 0.115},
		{1000, 1000 * 0.115},
	}
	for _, tt := range tests {
		got := c.awsStorageCost(tt.totalGB)
		if got != tt.want {
			t.Errorf("awsStorageCost(%v) = %v, want %v", tt.totalGB, got, tt.want)
		}
	}
}

func TestGCPStorageCost(t *testing.T) {
	c := &costCommand{}
	tests := []struct {
		totalGB float64
		want    float64
	}{
		{0, 0},
		{100, 100 * 0.170},
		{500, 500 * 0.170},
		{1000, 1000 * 0.170},
	}
	for _, tt := range tests {
		got := c.gcpStorageCost(tt.totalGB)
		if got != tt.want {
			t.Errorf("gcpStorageCost(%v) = %v, want %v", tt.totalGB, got, tt.want)
		}
	}
}

func TestSelfStorageCost(t *testing.T) {
	c := &costCommand{}
	tests := []struct {
		totalGB float64
		want    float64
	}{
		{0, 0},
		{100, 100 * 0.100},
		{500, 500 * 0.100},
		{1000, 1000 * 0.100},
	}
	for _, tt := range tests {
		got := c.selfStorageCost(tt.totalGB)
		if got != tt.want {
			t.Errorf("selfStorageCost(%v) = %v, want %v", tt.totalGB, got, tt.want)
		}
	}
}

func TestWorkerInstance(t *testing.T) {
	tests := []struct {
		concurrency int
		want        string
	}{
		{0, "t3.micro"},
		{5, "t3.micro"},
		{10, "t3.micro"},
		{11, "t3.small"},
		{25, "t3.small"},
		{26, "t3.medium"},
		{50, "t3.medium"},
		{51, "t3.large"},
		{100, "t3.large"},
		{101, "t3.xlarge"},
		{500, "t3.xlarge"},
	}
	for _, tt := range tests {
		c := &costCommand{concurrency: tt.concurrency}
		got := c.workerInstance()
		if got != tt.want {
			t.Errorf("workerInstance(concurrency=%d) = %q, want %q", tt.concurrency, got, tt.want)
		}
	}
}

func TestWorkerCost(t *testing.T) {
	c := &costCommand{}
	tests := []struct {
		workerCount float64
		want        float64
	}{
		{0, 0},
		{1, 1 * 0.083 * 730},
		{2, 2 * 0.083 * 730},
		{10, 10 * 0.083 * 730},
	}
	for _, tt := range tests {
		got := c.workerCost(tt.workerCount)
		if !approxEqual(got, tt.want) {
			t.Errorf("workerCost(%v) = %v, want %v (delta)", tt.workerCount, got, tt.want)
		}
	}
}

func TestCostFormat(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		contains []string
	}{
		{
			name:     "aws",
			provider: "aws",
			contains: []string{"Cleat Cost Estimate", "Workload:", "aws", "db.r6g", "t3.small", "Total:"},
		},
		{
			name:     "gcp",
			provider: "gcp",
			contains: []string{"Cleat Cost Estimate", "Workload:", "gcp", "db-custom", "t3.small", "Total:"},
		},
		{
			name:     "self",
			provider: "self",
			contains: []string{"Cleat Cost Estimate", "Workload:", "self", "self-hosted", "t3.small", "Total:"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &costCommand{
				workload:      100,
				avgDuration:   5.0,
				eventsPerWF:   10,
				retentionDays: 30,
				replication:   1,
				provider:      tt.provider,
				concurrency:   25,
			}
			output := c.Format()
			for _, want := range tt.contains {
				if !strings.Contains(output, want) {
					t.Errorf("expected output to contain %q, got:\n%s", want, output)
				}
			}
		})
	}
}

func TestCostEstimate(t *testing.T) {
	c := &costCommand{
		workload:      100,
		avgDuration:   5.0,
		eventsPerWF:   10,
		retentionDays: 30,
		replication:   1,
		provider:      "aws",
		concurrency:   25,
	}
	est := c.Estimate()

	if est.Workload != 100 {
		t.Errorf("Workload = %d, want 100", est.Workload)
	}
	if est.EventsPerSecond != 1000 {
		t.Errorf("EventsPerSecond = %v, want 1000", est.EventsPerSecond)
	}
	if est.WorkerCount != 20 {
		t.Errorf("WorkerCount = %d, want 20", est.WorkerCount)
	}
	if est.DBInstanceType != "db.r6g.large" {
		t.Errorf("DBInstanceType = %q, want db.r6g.large", est.DBInstanceType)
	}
	if est.WorkerInstanceType != "t3.small" {
		t.Errorf("WorkerInstanceType = %q, want t3.small", est.WorkerInstanceType)
	}
	if est.Provider != "aws" {
		t.Errorf("Provider = %q, want aws", est.Provider)
	}
	if est.TotalMonthlyCost <= 0 {
		t.Errorf("TotalMonthlyCost = %v, want > 0", est.TotalMonthlyCost)
	}
}

func TestCostEstimate_MinWorkerCount(t *testing.T) {
	c := &costCommand{
		workload:      1,
		avgDuration:   0.5,
		eventsPerWF:   5,
		retentionDays: 30,
		replication:   1,
		provider:      "aws",
		concurrency:   25,
	}
	est := c.Estimate()
	if est.WorkerCount < 1 {
		t.Errorf("WorkerCount = %d, want at least 1", est.WorkerCount)
	}
	if est.WorkerCount != 1 {
		t.Errorf("WorkerCount = %d, want 1 (minimum)", est.WorkerCount)
	}
}

func TestCostFormat_ZeroWorkload(t *testing.T) {
	c := &costCommand{
		workload:      0,
		avgDuration:   3.0,
		eventsPerWF:   15,
		retentionDays: 90,
		replication:   1,
		provider:      "aws",
		concurrency:   25,
	}
	output := c.Format()
	if !strings.Contains(output, "0 workflows/sec") {
		t.Errorf("expected zero workload in output, got:\n%s", output)
	}
	if !strings.Contains(output, "Total:") {
		t.Errorf("expected Total: in output, got:\n%s", output)
	}
}

func TestRunCost_FlagParsing(t *testing.T) {
	stderr := withExitPanic(t, func() {
		runCost([]string{})
	})
	// With default args, runCost should print the cost estimate and not panic.
	// withExitPanic captures stderr; since runCost doesn't call osExit,
	// the panic should NOT be triggered.
	if stderr != "" {
		// runCost prints to stdout, not stderr; if there's stderr it might be
		// from flag parsing errors - which shouldn't happen with defaults.
		t.Logf("unexpected stderr: %s", stderr)
	}
}

func TestRunCost_CustomFlags(t *testing.T) {
	// runCost with custom flags should produce output without panicking.
	stdout := captureStdout(t, func() {
		runCost([]string{"--workload", "500", "--provider", "gcp", "--concurrency", "50"})
	})
	if !strings.Contains(stdout, "500 workflows/sec") {
		t.Errorf("expected 500 workflows/sec in output, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "gcp") {
		t.Errorf("expected gcp provider in output, got:\n%s", stdout)
	}
}
