//go:build !unix

package testutil

// processExists answers "yes" for every pid where there is no portable way to ask. A sweep that cannot tell whether an owner is alive must
// not remove its directory, so on these platforms OwnedTempDir creates the directory and never sweeps.
func processExists(pid int) bool { return true }
