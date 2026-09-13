// Package sagaparameterised is the shape cleat#1131 was filed about: a saga
// whose steps are built from data rather than written out one by one.
//
// Every other way of parameterising a step is rejected. A local closure factory
// gets E009 ("function-value calls cannot be statically resolved"); a
// package-level factory gets "is reachable from a workflow entry point but does
// not have a HostCalls parameter", because the DurableCall inside its returned
// closure is attributed to the factory. This file is the form that works, and
// it exists so that a change making it stop working fails the build.
package sagaparameterised

import "github.com/cleat-team/cleat/cleat"

type transfer struct {
	op     string
	undo   string
	amount string
}

// HandleParameterisedSaga builds its steps in a LOOP, which is the thing the
// issue could not express. A StepCall is data.
func HandleParameterisedSaga(h cleat.HostCalls, account string, tag string) (string, error) {
	legs := []transfer{
		{op: "withdraw", undo: "refund", amount: `{"amt":10}`},
		{op: "deposit", undo: "reverse", amount: `{"amt":10}`},
		{op: "notify", undo: "", amount: `{"who":"ops"}`}, // no compensation
	}

	s := cleat.NewSaga()
	for _, leg := range legs {
		s.AddStepCall(cleat.StepCall{
			Description:  leg.op,
			Service:      "banking",
			Op:           leg.op,
			Payload:      leg.amount,
			CompensateOp: leg.undo,
		})
	}
	if err := s.Run(h); err != nil {
		return "", err
	}
	return `{"ok":true}`, nil
}
