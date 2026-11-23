import re,subprocess,json,struct,sys,os,random
def uleb(n):
    out=bytearray()
    while True:
        b=n&0x7f; n>>=7
        if n: out.append(b|0x80)
        else: out.append(b); return bytes(out)
def sleb(n):
    out=bytearray()
    while True:
        b=n&0x7f; n>>=7
        if (n==0 and not b&0x40) or (n==-1 and b&0x40): out.append(b); return bytes(out)
        out.append(b|0x80)
def module(pages,segs,maxpages=None):
    mem=b'\x01'+(b'\x00'+uleb(pages) if maxpages is None else b'\x01'+uleb(pages)+uleb(maxpages))
    d=uleb(len(segs))
    for off,data in segs:
        d+=b'\x00\x41'+sleb(off)+b'\x0b'+uleb(len(data))+data
    sec=lambda i,b:bytes([i])+uleb(len(b))+b
    return b'\0asm\1\0\0\0'+sec(5,mem)+sec(11,d)
# python segment list
dump=subprocess.run(['wasm-tools','dump','/tmp/wasm/py_main.wasm'],capture_output=True,text=True).stdout.split('\n')
segs=[];kind=None;off=None;ind=False
for l in dump:
    if '| data section' in l: ind=True; continue
    if ind and re.search(r'\|\s+(\w+) section',l) and 'data section' not in l: break
    if not ind: continue
    if re.search(r'\| data memory\[0\]',l): kind=1; off=None; continue
    m=re.search(r'\| i32_const value:(-?\d+)',l)
    if m and kind and off is None: off=int(m.group(1)); continue
    m2=re.search(r'\.\.\. (\d+) bytes of data',l)
    if m2 and kind: segs.append((off,int(m2.group(1)))); kind=None; off=None
print("python layout:",len(segs),"segments",file=sys.stderr)
random.seed(1)
fill=lambda n:bytes([0x41+(i%23) for i in range(min(n,4096))])*(n//4096+1)
def pat(n): return (fill(n))[:n]
out={}
out['S1_one_big_10.6MiB_363pages']=module(363,[(984392,pat(11138817))])
out['S2_replica_2539segs_same_offsets']=module(363,[(o,pat(n)) for o,n in segs])
out['S3_2539_tiny_segs_363pages']=module(363,[(o,pat(16)) for o,_ in segs])
out['S4_one_big_10.6MiB_200pages']=module(200,[(0,pat(11138817))])
out['S5_tiny_one_seg_363pages']=module(363,[(1024,pat(1000))])
out['S6_replica_with_max_32768']=module(363,[(o,pat(n)) for o,n in segs],32768)
out['S7_one_2MiB_51pages']=module(51,[(43968,pat(2*2**20))])
out['S8_one_5MiB_100pages']=module(100,[(0,pat(5*2**20))])
out['S9_one_8MiB_130pages']=module(130,[(0,pat(8*2**20))])
os.makedirs('/tmp/wasm/synth',exist_ok=True)
for k,v in out.items(): open(f'/tmp/wasm/synth/{k}.wasm','wb').write(v); print(k,len(v),file=sys.stderr)
