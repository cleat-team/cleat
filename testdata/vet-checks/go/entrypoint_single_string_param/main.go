// Package main is the fixture for W003: an entry point whose only parameter,
// after HostCalls, is a single string.
//
// Such a parameter receives the WHOLE input JSON rather than the field of that
// name. Starting this workflow with {"key": "lock-abc"} binds key to the
// literal text {"key":"lock-abc"}, so the lock key becomes
// lock-{"key":"lock-abc"} and every acquire fails with a message naming
// cleat_acquire_lock -- which is where this cost real time. cleat#824.
package main

import "github.com/cleat-team/cleat/cleat"

// HandleOneString takes the shape the warning is about.
func HandleOneString(h cleat.HostCalls, key string) (string, error) {
	acquired, err := h.AcquireLockMs("lock-"+key, 120000)
	if err != nil {
		return "", err
	}
	if !acquired {
		return `{"acquired":false}`, nil
	}
	return `{"acquired":true}`, nil
}

// HandleTwoStrings is the same workflow with a second parameter, which binds
// by name as every other shape does. It must NOT warn -- a check that fired on
// both would say nothing about the difference between them.
func HandleTwoStrings(h cleat.HostCalls, key string, tag string) (string, error) {
	acquired, err := h.AcquireLockMs("lock-"+key+"-"+tag, 120000)
	if err != nil {
		return "", err
	}
	if !acquired {
		return `{"acquired":false}`, nil
	}
	return `{"acquired":true}`, nil
}

// HandleOneStruct takes a struct, which is unmarshalled from the input JSON
// and binds by field. Also must not warn.
type Input struct {
	Key string `json:"key"`
}

func HandleOneStruct(h cleat.HostCalls, in Input) (string, error) {
	acquired, err := h.AcquireLockMs("lock-"+in.Key, 120000)
	if err != nil {
		return "", err
	}
	if !acquired {
		return `{"acquired":false}`, nil
	}
	return `{"acquired":true}`, nil
}

// HandleOneInt takes a single non-string parameter, which binds by name like
// any other. Only the single STRING case takes the whole payload.
func HandleOneInt(h cleat.HostCalls, timeoutMs int) (string, error) {
	h.DurableSleepMs(int64(timeoutMs))
	return `{"slept":true}`, nil
}
