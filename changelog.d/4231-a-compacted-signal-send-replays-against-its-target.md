---
section: fixed
---
- **A compacted history checked only the NAME of a recorded signal send, not the workflow it was sent to (cleat#4231).** Compaction kept a `signal_workflow` record's signal name, payload and outcome and dropped its target run, so a replay of a compacted send could not tell a send to another workflow from the recorded one, and a workflow that had changed which run it signals replayed the old send as its own. Compaction now carries the target (it is the record's `run_id` column, so no event checksum changes). A history compacted by an earlier build has no target on its send, and for those records the replay still compares the name only, as it did.
