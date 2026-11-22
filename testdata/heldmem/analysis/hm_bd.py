import json,sys,collections
def load(f):
    try: return [json.loads(l) for l in open(f) if l.strip()]
    except FileNotFoundError: return []
for f,tag in (("/tmp/hm_bd.jsonl","cow=1"),("/tmp/hm_bd_cow0.jsonl","cow=0")):
    g=collections.defaultdict(list)
    for r in load(f): g[r["lang"]].append(r)
    for lang,rs in sorted(g.items()):
        def m(fn): return sum(fn(r) for r in rs)/len(rs)
        n=rs[0]["n"]
        pre=lambda k:m(lambda r:r["pre"][k])/1024; bas=lambda k:m(lambda r:r["base"][k])/1024
        per=lambda k:m(lambda r:(r["held"][k]-r["base"][k])/r["n"])/1024
        print(f"{tag} {lang:10s} runs={len(rs)} N={n} | pre-warm-up Pss={pre('Pss'):5.0f} Anon={pre('Anon'):4.0f} | after warm-up Pss={bas('Pss'):5.0f} Anon={bas('Anon'):4.0f} ExecAnon={bas('ExecAnonRss'):4.0f} Memfd={bas('MemfdRss'):4.1f} | PER INSTANCE Anon={per('Anon'):5.2f} Memfd={per('MemfdRss'):5.2f} Shmem={per('Shmem'):5.2f} Pss={per('Pss'):5.2f} PrivDirty={per('PrivateDirty'):5.2f} MiB")
