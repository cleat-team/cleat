import re,sys,subprocess
dump=subprocess.run(['wasm-tools','dump',sys.argv[1]],capture_output=True,text=True).stdout.split('\n')
# find the data section and parse each segment: "data memory[0]" / "data passive" then offset expr, then "N bytes of data"
segs=[];i=0;kind=None
in_data=False
for k,l in enumerate(dump):
    if '| data section' in l: in_data=True; continue
    if in_data and re.search(r'\|\s+(\w+) section',l) and 'data section' not in l: break
    if not in_data: continue
    m=re.search(r'\| data (memory\[(\d+)\]|passive)',l)
    if m: kind=m.group(1); off=None; continue
    m=re.search(r'\| (i32|i64)_const value:(-?\d+)',l)
    if m and off is None and kind: off=int(m.group(2)); continue
    m=re.search(r'\| global_get|\| i32_const|\| i32.add',l)
    m2=re.search(r'\|\s*-+\|\s*\.\.\. (\d+) bytes of data',l)
    if m2 and kind is not None:
        segs.append((kind,off,int(m2.group(1)))); kind=None; off=None
print("segments:",len(segs)," passive:",sum(1 for s in segs if s[0]=='passive')," no-const-offset:",sum(1 for s in segs if s[0]!='passive' and s[1] is None))
act=[s for s in segs if s[0]!='passive' and s[1] is not None]
tot=sum(s[2] for s in act); print("active const-offset segments:",len(act)," total bytes:",tot,"=%.2f MiB"%(tot/2**20))
print("min offset:",min(s[1] for s in act)," max end:",max(s[1]+s[2] for s in act)," = %.2f MiB"%(max(s[1]+s[2] for s in act)/2**20))
srt=sorted(act,key=lambda s:s[1]); ov=sum(1 for a,b in zip(srt,srt[1:]) if a[1]+a[2]>b[1])
print("overlapping adjacent pairs (after sorting):",ov, " already in offset order:",all(a[1]<=b[1] for a,b in zip(act,act[1:])))
span=max(s[1]+s[2] for s in act)-min(s[1] for s in act); print("span %.2f MiB, bytes in segments %.2f MiB, density %.0f%%"%(span/2**20,tot/2**20,100*tot/span))
big=sorted(act,key=lambda s:-s[2])[:3]; print("largest segments:",[(s[1],s[2]) for s in big])
