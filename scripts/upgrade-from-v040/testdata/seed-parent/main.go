// seed-parent starts a seed-waiter child that waits on a signal, and awaits
// it. A parent and child both in flight when the v0.4.0 worker stops is the
// shape the workflow-instances split touches most. cleat#3386.
package seedparent

import (
	"encoding/json"
	"fmt"

	"github.com/cleat-team/cleat/cleat"
)

type input struct {
	N int `json:"n"`
}

func Parent(h cleat.HostCalls, raw string) (string, error) {
	var in input
	_ = json.Unmarshal([]byte(raw), &in)

	h.SetQueryState("stage", "spawning")
	childIn, _ := json.Marshal(map[string]any{"n": in.N, "mode": "signal"})
	rid, err := h.ChildWorkflow("seed-waiter", string(childIn))
	if err != nil {
		return "", err
	}
	h.SetQueryState("child", rid)
	h.SetQueryState("stage", "awaiting-child")
	res, err := h.AwaitChild(rid)
	if err != nil {
		return "", err
	}
	h.SetQueryState("stage", "done")
	return fmt.Sprintf(`{"n":%d,"child":%q}`, in.N, res), nil
}
