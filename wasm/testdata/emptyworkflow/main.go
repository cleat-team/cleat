// Package emptyworkflow is the source the adapter-compile guard analyses.
//
// It deliberately makes NO host calls. The guard supplies its own UsageInfo
// covering every adapter definition, and a workflow that called some of them
// would only narrow coverage to whichever ones it happened to use -- which is
// the blind spot the guard exists to close.
//
// It is a real workflow rather than an empty package because GenerateExports
// builds its dispatch from real entry points, and an exports file with none
// declares a HostCalls value nothing uses.
package emptyworkflow

import "github.com/cleat-team/cleat/cleat"

//cleat:entry
func Probe(h cleat.HostCalls, input string) (string, error) {
	return input, nil
}
