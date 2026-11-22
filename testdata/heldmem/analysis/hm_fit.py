#!/usr/bin/env python3
"""Fit physical memory = shared + N x per-workflow, per (lang, mode, metric), over every run in the given jsonl files. Pure python (no numpy)."""
import json, sys, collections, math
rows=[json.loads(l) for f in sys.argv[1:] for l in open(f) if l.strip()]
def fit(xs, ys):
    n=len(xs); mx=sum(xs)/n; my=sum(ys)/n
    sxx=sum((x-mx)**2 for x in xs); sxy=sum((x-mx)*(y-my) for x,y in zip(xs,ys))
    b=sxy/sxx; a=my-b*mx
    ssr=sum((y-(a+b*x))**2 for x,y in zip(xs,ys)); sst=sum((y-my)**2 for y in ys)
    r2=1-ssr/sst if sst else 1.0
    res=[y-(a+b*x) for x,y in zip(xs,ys)]
    return a,b,r2,max(abs(r) for r in res)
METRICS={'Pss':lambda s:s['Pss'],'Rss':lambda s:s['Rss'],'Anon':lambda s:s['Anon'],'Shmem':lambda s:s['Shmem'],'PrivateDirty':lambda s:s['PrivateDirty'],
         'Threads':lambda s:s['Threads'],'StackRss':lambda s:s['StackRss'],'GoHeap':lambda s:s['GoHeap']}
g=collections.defaultdict(list)
for r in rows: g[(r['lang'],r['mode'],r['calls'],r['mb'])].append(r)
for key in sorted(g):
    rs=g[key]; lang,mode,calls,mb=key
    print(f"\n=== {lang} {mode} calls={calls} mb={mb}: {len(rs)} runs, N in {sorted(set(r['n'] for r in rs))}, reps/N {sorted(collections.Counter(r['n'] for r in rs).values())[:1]}..")
    base=[r['base']['Pss'] for r in rs]; print(f"  baseline after warm-up (before any hold): Pss {min(base)/1024:.0f}-{max(base)/1024:.0f} MiB  Rss {min(r['base']['Rss'] for r in rs)/1024:.0f}-{max(r['base']['Rss'] for r in rs)/1024:.0f} MiB  Anon {min(r['base']['Anon'] for r in rs)/1024:.0f}-{max(r['base']['Anon'] for r in rs)/1024:.0f} MiB")
    for name,f in METRICS.items():
        xs=[r['n'] for r in rs]
        for label,ys in (('delta (held - base)',[f(r['held'])-f(r['base']) for r in rs]),):
            a,b,r2,mr=fit(xs,ys)
            unit='MiB' if name not in ('Threads',) else ''
            div=1024 if name not in ('Threads',) else 1
            print(f"  {name:12s} {label}: shared={a/div:8.2f} {unit}  per-workflow={b/div:7.3f} {unit}  R2={r2:.4f}  max|resid|={mr/div:.2f}")
    # nonlinearity: per-N mean of Pss delta per instance
    byn=collections.defaultdict(list)
    for r in rs: byn[r['n']].append((r['held']['Pss']-r['base']['Pss'])/r['n']/1024)
    print("  Pss delta/N by N (MiB, mean of reps; min..max): "+"  ".join(f"N={n}:{sum(v)/len(v):.2f}({min(v):.2f}..{max(v):.2f})" for n,v in sorted(byn.items())))
    comp=set((r['compiles_component'],r['compiles_core']) for r in rs); print(f"  compilations (component, core) over whole process incl. warm-up: {sorted(comp)}  held_check==n: {all(r['held_check']==r['n'] for r in rs)}")
