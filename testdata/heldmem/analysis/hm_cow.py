import json,collections
def load(f): return [json.loads(l) for l in open(f) if l.strip()]
def fit(xs,ys):
    n=len(xs); mx=sum(xs)/n; my=sum(ys)/n
    b=sum((x-mx)*(y-my) for x,y in zip(xs,ys))/sum((x-mx)**2 for x in xs); a=my-b*mx
    ssr=sum((y-(a+b*x))**2 for x,y in zip(xs,ys)); sst=sum((y-my)**2 for y in ys)
    return a,b,(1-ssr/sst if sst else 1)
c1=collections.defaultdict(list); c0=collections.defaultdict(list)
for r in load('/tmp/hm_cow1.jsonl'): c1[r['lang']].append(r)
for r in load('/tmp/hm_cow0.jsonl'): c0[r['lang']].append(r)
print("hold mode, zero data, N in 1,4,16,32; per-workflow slope of PHYSICAL memory (Pss, MiB) and of Shmem (MiB, as Rss counts it)")
for lang in ('go','javascript','python','csharp'):
    row=[]
    for tag,g in (('CoW ON (default)',c1),('CoW OFF',c0)):
        rs=g.get(lang,[])
        if not rs: row.append(f"{tag}: no rows (the run aborts)"); continue
        xs=[r['n'] for r in rs]
        a,b,r2=fit(xs,[(r['held']['Pss']-r['base']['Pss'])/1024 for r in rs])
        a2,b2,_=fit(xs,[(r['held']['Shmem']-r['base']['Shmem'])/1024 for r in rs])
        row.append(f"{tag}: Pss slope {b:6.2f} (shared {a:5.2f}, R2 {r2:.4f}, {len(rs)} runs), Shmem slope {b2:5.2f}")
    print(f"{lang:10s} | "+" | ".join(row))
