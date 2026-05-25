// Package testfixture provides a lightweight integration test harness for the
// clew-service HTTP API. It boots an in-process HTTP server with all registered
// plugin routes, suitable for smoke and integration tests.
//
// Usage:
//
//	fx := testfixture.New(t)
//	defer fx.Close()
//	resp, _ := http.Get(fx.URL + "/api/projects")
package testfixture
