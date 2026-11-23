---
section: upgrade
---
- **A promise call's store failure is recorded from this version on; histories recorded before it keep answering success (cleat#4335).** An event written by an earlier version carries no failure, so a replay of one still answers success for a `create_promise`, `resolve_promise` or `reject_promise` whose store write had failed on the original run, as it always did. Only calls made after the upgrade are recorded with their outcome. During a rolling upgrade an older worker that replays a history written by a newer one also answers success for such a call, because it does not read the new field; a workflow that branches on the error from one of these calls can therefore still see the two answers differ until every worker is upgraded.
