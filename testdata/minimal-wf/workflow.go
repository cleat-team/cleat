package main

import "github.com/cleat-team/cleat/cleat"

func SimpleWorkflow(h cleat.HostCalls, input string) (string, error) {
	// Tight loop: 1000 DurableCalls, no external HTTP needed
	for i := 0; i < 1000; i++ {
		if _, err := h.DurableCall("increment", "counter", "1"); err != nil {
			return "", err
		}
	}
	return "ok", nil
}

func SimpleWorkflowShort(h cleat.HostCalls, input string) (string, error) {
	for i := 0; i < 10; i++ {
		if _, err := h.DurableCall("increment", "counter", "1"); err != nil {
			return "", err
		}
	}
	return "ok", nil
}
