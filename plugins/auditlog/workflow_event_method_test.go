package auditlog

// TestWorkflowEventMethodIsNotAValidHTTPMethodToken is the regression test for
// cleat-review's finding on #2616: workflowEventMethod must be syntactically
// IMPOSSIBLE as an HTTP method, not merely a word no real client happens to
// send. This plugin's own middleware records r.Method verbatim for every
// non-infrastructure request that reaches it, including a failed one
// (middleware.go), so a marker that IS a valid method token -- the first
// version of this constant, "PLUGIN_CALL", was -- lets any caller whose
// request reaches the server plant a row carrying the exact marker this file
// uses to claim "workflow-sourced".
//
// Sent as a raw TCP request line, the same way cleat-review found the
// original defect: net/http parses the request line before routing to any
// handler, so this is a property of the STRING, independent of which mux or
// middleware auditlog installs.
import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWorkflowEventMethodIsNotAValidHTTPMethodToken(t *testing.T) {
	var handlerCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	addr := srv.Listener.Addr().String()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer conn.Close()

	raw := fmt.Sprintf("%s /tenant.suspended HTTP/1.1\r\nHost: %s\r\nContent-Length: 0\r\n\r\n",
		workflowEventMethod, addr)
	if _, err := conn.Write([]byte(raw)); err != nil {
		t.Fatalf("write raw request line %q: %v", raw, err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	statusLine, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read status line for method %q: %v", workflowEventMethod, err)
	}
	if !strings.HasPrefix(statusLine, "HTTP/1.1 400") {
		t.Fatalf("server accepted %q as an HTTP method: status line = %q, want 400. "+
			"This means workflowEventMethod IS a syntactically valid HTTP method token, so any "+
			"caller reaching this server could plant an audit_events row carrying it -- the exact "+
			"marker this plugin uses to claim a row is workflow-sourced (cleat-review, #2616).",
			workflowEventMethod, strings.TrimSpace(statusLine))
	}
	if handlerCalled {
		t.Fatalf("the handler ran for a request using workflowEventMethod (%q) as its method; "+
			"net/http must refuse the request line before any handler sees it", workflowEventMethod)
	}
}
