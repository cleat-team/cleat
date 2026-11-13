package main

import (
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/cleat-team/cleat/engine"
)

// stateFilterPrefix is the query-parameter prefix of the published-state filter on GET /api/workflows:
// `?state.order_id=123` selects the runs that published order_id=123. cleat#3775.
const stateFilterPrefix = "state."

// queryStateFilterFromParams reads the `state.<key>=<value>` parameters. It refuses what it cannot honour
// rather than ignoring it, because an ignored filter is a wider answer the caller reads as a narrow one
// (cleat#1172 measured exactly that for concurrency_key):
//
//   - `state=...` with no key names no filter, and would be dropped as an unknown parameter;
//   - the same key twice is two equalities that can never both hold, so the answer would be empty for a
//     reason the caller did not intend;
//   - more than engine.MaxQueryStateFilterPairs pairs.
//
// An empty value is a value (`state.status=` selects runs that published an empty status), and an empty
// key (`state.=v`) is a key, because a workflow can publish under "".
func queryStateFilterFromParams(q url.Values) (map[string]string, error) {
	if _, bare := q["state"]; bare {
		return nil, fmt.Errorf("state needs a key: use state.<key>=<value>, for example state.order_id=123")
	}
	var pairs map[string]string
	names := make([]string, 0, len(q))
	for name := range q {
		if strings.HasPrefix(name, stateFilterPrefix) {
			names = append(names, name)
		}
	}
	sort.Strings(names) // a stable error for a request that breaks two rules
	for _, name := range names {
		vals := q[name]
		if len(vals) > 1 {
			return nil, fmt.Errorf("%s was given %d times; a key takes one value", name, len(vals))
		}
		if pairs == nil {
			pairs = make(map[string]string, len(names))
		}
		pairs[strings.TrimPrefix(name, stateFilterPrefix)] = vals[0]
	}
	if len(pairs) > engine.MaxQueryStateFilterPairs {
		return nil, fmt.Errorf("%w (got %d)", engine.ErrQueryStateFilterTooManyPairs, len(pairs))
	}
	return pairs, nil
}
