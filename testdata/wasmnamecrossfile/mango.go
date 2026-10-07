package main

import "github.com/cleat-team/cleat/cleat"

// Zookeeper sorts alphabetically after Aardvark (zebra.go) -- see zebra.go's
// comment for why the two are split across files this way.
func Zookeeper(h cleat.HostCalls, input string) (string, error) {
	return input, nil
}
