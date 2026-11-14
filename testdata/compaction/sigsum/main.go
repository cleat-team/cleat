// Package sigsum is a fixture for cleat#3843: a workflow whose RESULT depends on the values it received, so a
// replay that is handed the wrong recorded history gives a wrong answer and not an error.
//
// That is the property that matters. cleat#3838 (a run compacted twice replayed the wrong events) finished
// "done" with 1713 for a correct 1335 and reported nothing; a workflow of timed-out awaits, which the first
// attempt at this test used, finished correctly on the broken code because no recorded value fed its result.
// It awaits one signal at a time and sums the payloads, so every resume replays everything received so far.
package sigsum

import (
	"fmt"
	"strconv"
	"time"

	"github.com/cleat-team/cleat/cleat"
)

//cleat:entry
func SigSum(h cleat.HostCalls, input string) (string, error) {
	sum := 0
	for i := 0; i < 30; i++ {
		r := h.AwaitSignals([]string{"n"}, 120*time.Second)
		n, err := strconv.Atoi(r.Payload)
		if err != nil {
			return "", fmt.Errorf("signal %d: payload %q: %v", i, r.Payload, err)
		}
		sum += n
	}
	return fmt.Sprintf(`{"sum":%d,"count":30}`, sum), nil
}
