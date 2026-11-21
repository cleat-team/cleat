---
section: fixed
---
- **A Java guest stops at a child await that must suspend, as it already did at `sleep` (cleat#4285).** `awaitChild`, `awaitAllChildren` and `awaitAnyChild` answered the host's suspend word (`1<<62`) with `CleatResult.err("... workflow must suspend")`, an ordinary value, so a guest that handled the value and carried on made its next call, which the host recorded after the pending await and ran a second time on the resume. They now throw `SuspendSignal`. A guest that tested the old error to detect "the child is not done yet" no longer sees it, because the call does not return in that case; poll with `pollChild` for a non-blocking check. This is step 2 of #4285; the host-side refusal of fresh work after a suspend, which covers every SDK, is separate.
