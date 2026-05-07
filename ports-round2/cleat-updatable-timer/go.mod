module github.com/rcownie/cleat-updatable-timer

go 1.26

// The Cleat Go SDK lives at github.com/rcownie/durable,
// with the main package at github.com/rcownie/durable/durable
// and the test harness at github.com/rcownie/durable/durable/durabletest.
require github.com/rcownie/durable v0.0.0

// Replace with the path to the cleat-agent1 root, where the durable module lives.
replace github.com/rcownie/durable => ../../
