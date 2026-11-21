---
section: fixed
---
- **Replay's refusal of a call recorded after a pending child await now covers a compacted history exactly where it cannot tell (cleat#4323).** The refusal (cleat#4285) exempted every record expanded from a compaction state, because compaction used to drop the drain stamps the refusal reads. Compaction now carries those stamps (cleat#4310) and records which part of a state carries them (`stamped_from` in the stored compaction state, absent in a state written before it), so only the records of a state compacted by an earlier build stay exempt. A history compacted by this build that holds a call after a pending child await is now refused loudly, as an uncompacted one is, where it used to be replayed quietly and the call ran again.
