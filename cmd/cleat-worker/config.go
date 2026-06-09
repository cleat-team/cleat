package main

import "time"

// Config holds structured configuration for the cleat worker.
type Config struct {
	DBURL               string
	Concurrency         int
	HeartbeatInterval   time.Duration
	PollInterval        time.Duration
	APIAddr             string
	TaskQueues          []string
	CompactionThreshold int
	CompactionInterval  time.Duration
	ShardsFile          string
	PluginConfigFile    string
	RequireAuth         bool
	RequireSignalAuth   bool
	MaxBodySize         int64
	MaxAttempts         int
	WASMCacheMaxEntries int
	WASMCacheMaxBytes   int64
	RedactPatternsFile  string
	DBMaxOpenConns      int
	DBMaxIdleConns      int
	LogLevel            string
	LogFormat           string
	OTelEndpoint        string
	OTelDisabled        bool
	MigrationsDir       string
}
