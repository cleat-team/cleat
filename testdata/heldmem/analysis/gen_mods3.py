import sys
exec(open('/tmp/gen_mods.py').read().split("random.seed(1)")[0])
fill=lambda n:bytes([0x41+(i%23) for i in range(min(n,4096))])*(n//4096+1)
def pat(n): return (fill(n))[:n]
import os,subprocess
os.makedirs('/tmp/wasm/bis',exist_ok=True)
def has_image(k):
    f=f'/tmp/wasm/bis/p{k}.wasm'; open(f,'wb').write(module(363,[(o,pat(n)) for o,n in segs[:k]]))
    import json
    out='/tmp/bis_out.jsonl'
    if os.path.exists(out): os.remove(out)
    env=dict(os.environ,GOFLAGS='-buildvcs=false',HELDMEM_IMAGE_WASM=f,HELDMEM_N='4',HELDMEM_IMAGE_OUT=out)
    subprocess.run(['/tmp/engine9.test','-test.run','^TestMeasureStandaloneImage$','-test.count=1'],cwd='/src/engine',env=env,capture_output=True)
    r=json.loads(open(out).readline()); return r['held']['MemfdMaps']-r['base']['MemfdMaps']>=4
lo,hi=2048,len(segs)   # has_image(lo)=True, has_image(hi)=False
assert has_image(lo) and not has_image(hi)
while hi-lo>1:
    mid=(lo+hi)//2
    if has_image(mid): lo=mid
    else: hi=mid
o,n=segs[hi-1]
print(f"first failing prefix: {hi} segments. The segment that breaks it is #{hi-1}: offset={o} ({o/2**20:.3f} MiB) length={n} end={o+n} ({(o+n)/2**20:.3f} MiB)")
print(f"last passing prefix {lo}: its last segment ends at {segs[lo-1][0]+segs[lo-1][1]} ({(segs[lo-1][0]+segs[lo-1][1])/2**20:.3f} MiB)")
print("memory minimum bytes:",363*65536, " 16 MiB =",16*2**20)
