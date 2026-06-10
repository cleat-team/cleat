package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

// costCommand implements the "cleatctl cost" subcommand for estimating
// monthly operational costs of running cleat in production.
//
// Usage:
//
//	cleatctl cost [flags]
//
// All parameters have defaults. At minimum, specify --workload (workflows/sec)
// to get an estimate.
type costCommand struct {
	workload      int     // workflows per second
	avgDuration   float64 // average workflow execution time in seconds
	eventsPerWF   int     // average events per workflow
	retentionDays int     // event history retention in days
	replication   int     // storage replication factor
	provider      string  // "aws", "gcp", "self"
	concurrency   int     // worker concurrency
}

func runCost(args []string) {
	fs := flag.NewFlagSet("cost", flag.ContinueOnError)
	c := &costCommand{
		workload:      10,
		avgDuration:   3.0,
		eventsPerWF:   15,
		retentionDays: 90,
		replication:   1,
		provider:      "aws",
		concurrency:   25,
	}
	fs.IntVar(&c.workload, "workload", c.workload, "Workflows per second")
	fs.Float64Var(&c.avgDuration, "avg-duration", c.avgDuration, "Average workflow execution time (seconds)")
	fs.IntVar(&c.eventsPerWF, "events-per-wf", c.eventsPerWF, "Average events per workflow execution")
	fs.IntVar(&c.retentionDays, "retention-days", c.retentionDays, "Event history retention in days")
	fs.IntVar(&c.replication, "replication", c.replication, "Storage replication factor (1=single, 2=multi-AZ)")
	fs.StringVar(&c.provider, "provider", c.provider, "Cloud provider: aws, gcp, self")
	fs.IntVar(&c.concurrency, "concurrency", c.concurrency, "Worker concurrency")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "error parsing flags: %v\n", err)
		osExit(1)
		return
	}

	fmt.Println(c.Format())
}

// Estimate returns a CostEstimate for the current parameters.
func (c *costCommand) Estimate() *CostEstimate {
	est := &CostEstimate{
		Workload:          c.workload,
		AvgDuration:       c.avgDuration,
		EventsPerWF:       c.eventsPerWF,
		RetentionDays:     c.retentionDays,
		Replication:       c.replication,
		Provider:          c.provider,
		WorkerConcurrency: c.concurrency,
	}

	// Compute derived values.
	eventsPerSec := float64(c.workload * c.eventsPerWF)
	est.EventsPerSecond = eventsPerSec
	est.DailyStorageGB = eventsPerSec * 86400 * 400 / (1024 * 1024 * 1024) // 400 bytes per event
	est.MonthlyStorageGB = est.DailyStorageGB * 30
	est.TotalStorageGB = est.MonthlyStorageGB * float64(c.retentionDays) / 30.0 * float64(c.replication)

	// Estimate workers needed.
	est.WorkerCount = c.workload * int(c.avgDuration) / c.concurrency
	if est.WorkerCount < 1 {
		est.WorkerCount = 1
	}

	// DB instance cost.
	switch c.provider {
	case "aws":
		est.DBInstanceType = c.awsDBInstance()
		est.DBMonthlyCost = c.awsDBCost()
		est.StorageMonthlyCost = c.awsStorageCost(est.TotalStorageGB)
	case "gcp":
		est.DBInstanceType = c.gcpDBInstance()
		est.DBMonthlyCost = c.gcpDBCost()
		est.StorageMonthlyCost = c.gcpStorageCost(est.TotalStorageGB)
	case "self":
		est.DBInstanceType = c.selfDBInstance()
		est.DBMonthlyCost = c.selfDBCost()
		est.StorageMonthlyCost = c.selfStorageCost(est.TotalStorageGB)
	}

	// Worker compute cost.
	est.WorkerInstanceType = c.workerInstance()
	est.WorkerMonthlyCost = c.workerCost(float64(est.WorkerCount))

	est.TotalMonthlyCost = est.DBMonthlyCost + est.StorageMonthlyCost + est.WorkerMonthlyCost

	return est
}

// Format returns a human-readable cost estimate.
func (c *costCommand) Format() string {
	est := c.Estimate()

	var b strings.Builder
	b.WriteString("=== Cleat Cost Estimate ===\n\n")
	b.WriteString("Input Parameters:\n")
	b.WriteString(fmt.Sprintf("  Workload:            %d workflows/sec\n", est.Workload))
	b.WriteString(fmt.Sprintf("  Avg execution:       %.1f seconds\n", est.AvgDuration))
	b.WriteString(fmt.Sprintf("  Events per workflow: %d\n", est.EventsPerWF))
	b.WriteString(fmt.Sprintf("  Retention:           %d days\n", est.RetentionDays))
	b.WriteString(fmt.Sprintf("  Replication:         %dx\n", est.Replication))
	b.WriteString(fmt.Sprintf("  Provider:            %s\n", est.Provider))
	b.WriteString(fmt.Sprintf("  Worker concurrency:  %d\n\n", est.WorkerConcurrency))

	b.WriteString("Derived Values:\n")
	b.WriteString(fmt.Sprintf("  Events/sec:         %.0f\n", est.EventsPerSecond))
	b.WriteString(fmt.Sprintf("  Daily storage:      %.2f GB\n", est.DailyStorageGB))
	b.WriteString(fmt.Sprintf("  Monthly storage:    %.2f GB\n", est.MonthlyStorageGB))
	b.WriteString(fmt.Sprintf("  Retained storage:   %.2f GB (incl. replication %dx)\n", est.TotalStorageGB, est.Replication))
	b.WriteString(fmt.Sprintf("  Workers needed:     %d\n\n", est.WorkerCount))

	b.WriteString("Cost Breakdown:\n")
	b.WriteString(fmt.Sprintf("  DB instance (%s):     $%.0f/month\n", est.DBInstanceType, est.DBMonthlyCost))
	b.WriteString(fmt.Sprintf("  Storage:               $%.0f/month\n", est.StorageMonthlyCost))
	b.WriteString(fmt.Sprintf("  Workers (%s):          $%.0f/month (x%d instances)\n",
		est.WorkerInstanceType, est.WorkerMonthlyCost, est.WorkerCount))
	b.WriteString(fmt.Sprintf("  ----------------------------------------\n"))
	b.WriteString(fmt.Sprintf("  Total:                 $%.0f/month\n", est.TotalMonthlyCost))

	return b.String()
}

const (
	awsRDSHourlyRate = 0.479 // db.r6g.xlarge
	awsStoragePrice  = 0.115 // gp3 per GB-month
	gcpVCPUPrice     = 0.047 // per vCPU per hour on committed use
	gcpRAMPrice      = 0.006 // per GB per hour on committed use
	gcpStoragePrice  = 0.170 // SSD per GB-month
	selfVMPrice      = 0.050 // approximate per vCPU-hour
	selfStoragePrice = 0.100 // approximate per GB-month
	workerHourly     = 0.083 // t3.large
)

// awsDBInstance returns the estimated DB instance type for the given workload.
func (c *costCommand) awsDBInstance() string {
	switch {
	case c.workload <= 1000:
		return "db.r6g.large"
	case c.workload <= 10000:
		return "db.r6g.xlarge"
	case c.workload <= 50000:
		return "db.r6g.2xlarge"
	case c.workload <= 200000:
		return "db.r6g.4xlarge"
	default:
		return "db.r6g.8xlarge"
	}
}

func (c *costCommand) gcpDBInstance() string {
	switch {
	case c.workload <= 1000:
		return "db-custom-2-8"
	case c.workload <= 10000:
		return "db-custom-4-16"
	case c.workload <= 50000:
		return "db-custom-8-32"
	case c.workload <= 200000:
		return "db-custom-16-64"
	default:
		return "db-custom-32-128"
	}
}

func (c *costCommand) selfDBInstance() string {
	return "self-hosted (8 vCPU, 32 GB)"
}

func (c *costCommand) awsDBCost() float64 {
	// Approximate hourly rates for r6g instances (us-east-1, 1yr reserved).
	rates := map[string]float64{
		"db.r6g.large":   0.239,
		"db.r6g.xlarge":  0.479,
		"db.r6g.2xlarge": 0.958,
		"db.r6g.4xlarge": 1.916,
		"db.r6g.8xlarge": 3.832,
	}
	if rate, ok := rates[c.awsDBInstance()]; ok {
		return rate * 730
	}
	return 0.479 * 730
}

func (c *costCommand) gcpDBCost() float64 {
	// Custom machine types: vCPU + RAM pricing.
	type gcpSpec struct {
		vCPU int
		RAM  int
	}
	specs := map[string]gcpSpec{
		"db-custom-2-8":    {2, 8},
		"db-custom-4-16":   {4, 16},
		"db-custom-8-32":   {8, 32},
		"db-custom-16-64":  {16, 64},
		"db-custom-32-128": {32, 128},
	}
	spec, ok := specs[c.gcpDBInstance()]
	if !ok {
		spec = gcpSpec{4, 16}
	}
	hourly := float64(spec.vCPU)*gcpVCPUPrice + float64(spec.RAM)*gcpRAMPrice
	return hourly * 730
}

func (c *costCommand) selfDBCost() float64 {
	// Self-hosted estimate: 8 vCPU VM + overhead.
	return 400.0
}

func (c *costCommand) awsStorageCost(totalGB float64) float64 {
	return totalGB * awsStoragePrice
}

func (c *costCommand) gcpStorageCost(totalGB float64) float64 {
	return totalGB * gcpStoragePrice
}

func (c *costCommand) selfStorageCost(totalGB float64) float64 {
	return totalGB * selfStoragePrice
}

func (c *costCommand) workerInstance() string {
	switch {
	case c.concurrency <= 10:
		return "t3.micro"
	case c.concurrency <= 25:
		return "t3.small"
	case c.concurrency <= 50:
		return "t3.medium"
	case c.concurrency <= 100:
		return "t3.large"
	default:
		return "t3.xlarge"
	}
}

func (c *costCommand) workerCost(workerCount float64) float64 {
	return workerCount * workerHourly * 730
}

// CostEstimate holds the output of a cost calculation.
type CostEstimate struct {
	Workload          int
	AvgDuration       float64
	EventsPerWF       int
	RetentionDays     int
	Replication       int
	Provider          string
	WorkerConcurrency int

	EventsPerSecond  float64
	DailyStorageGB   float64
	MonthlyStorageGB float64
	TotalStorageGB   float64
	WorkerCount      int

	DBInstanceType     string
	DBMonthlyCost      float64
	StorageMonthlyCost float64
	WorkerInstanceType string
	WorkerMonthlyCost  float64
	TotalMonthlyCost   float64
}
