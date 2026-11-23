import sys
sys.argv=['x']
exec(open('/tmp/gen_mods.py').read().split("random.seed(1)")[0])
fill=lambda n:bytes([0x41+(i%23) for i in range(min(n,4096))])*(n//4096+1)
def pat(n): return (fill(n))[:n]
import os
os.makedirs('/tmp/wasm/synth2',exist_ok=True)
w=lambda name,m: open(f'/tmp/wasm/synth2/{name}.wasm','wb').write(m)
for k in (2,8,32,128,256,512,1024,2048):
    w(f'P_first{k:04d}_segments',module(363,[(o,pat(n)) for o,n in segs[:k]]))
# gap-free: 2539 adjacent segments, same sizes, starting at the same first offset
off=segs[0][0]; adj=[]
for o,n in segs:
    adj.append((off,pat(n))); off+=n
w('C_2539_adjacent_no_gaps',module(363,adj))
# same count, tiny, adjacent
off=segs[0][0]; t=[]
for i in range(len(segs)): t.append((off,pat(16))); off+=16
w('C_2539_tiny_adjacent',module(363,t))
# 1 segment covering the whole span (zero-padded): the 'merged' form
span_lo=segs[0][0]; span_hi=segs[-1][0]+segs[-1][1]
w('M_one_merged_segment_full_span',module(363,[(span_lo,pat(span_hi-span_lo))]))
