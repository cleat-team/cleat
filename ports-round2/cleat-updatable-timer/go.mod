module github.com/rcownie/cleat-updatable-timer

go 1.26

// The Cleat Go SDK lives at github.com/rcownie/cleat,
// with the main package at github.com/rcownie/cleat/durable
// and the test harness at github.com/rcownie/cleat/durable/durabletest.
require github.com/rcownie/cleat v0.0.0

// Replace with the path to the cleat-agent1 root, where the durable module lives.
replace github.com/rcownie/cleat => ../../
