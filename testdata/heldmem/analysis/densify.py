#!/usr/bin/env python3
"""Merge a core module's active data segments into ONE dense segment (zero-filled gaps): the same final memory contents. Then, optionally, splice the module into a component."""
import sys,struct
def uleb(n):
    o=bytearray()
    while True:
        b=n&0x7f; n>>=7
        if n: o.append(b|0x80)
        else: o.append(b); return bytes(o)
def rd_uleb(b,i):
    r=s=0
    while True:
        x=b[i]; i+=1; r|=(x&0x7f)<<s; s+=7
        if not x&0x80: return r,i
def rd_sleb(b,i):
    r=s=0
    while True:
        x=b[i]; i+=1; r|=(x&0x7f)<<s; s+=7
        if not x&0x80:
            if x&0x40: r-=1<<s
            return r,i
def sections(b,start):
    i=start; out=[]
    while i<len(b):
        sid=b[i]; sz,j=rd_uleb(b,i+1); out.append((sid,i,j,j+sz)); i=j+sz
    return out
def densify_module(m):
    secs=sections(m,8); out=bytearray(m[:8]); stats={}
    for sid,start,pstart,pend in secs:
        payload=m[pstart:pend]
        if sid==11:
            cnt,i=rd_uleb(payload,0); segs=[]
            for _ in range(cnt):
                flag,i=rd_uleb(payload,i); assert flag==0,f"flag {flag}: not an active memory-0 segment"
                assert payload[i]==0x41; off,i=rd_sleb(payload,i+1); assert payload[i]==0x0b; i+=1
                n,i=rd_uleb(payload,i); segs.append((off&0xffffffff,payload[i:i+n])); i+=n
            assert i==len(payload)
            lo=min(o for o,_ in segs); hi=max(o+len(d) for o,d in segs)
            buf=bytearray(hi-lo)
            for o,d in sorted(segs,key=lambda s:s[0]): buf[o-lo:o-lo+len(d)]=d   # in file order = sorted (checked: no overlaps)
            new=uleb(1)+b'\x00\x41'+(lambda v:(lambda o:o)(bytes(_sl(v))))(lo)+b'\x0b'+uleb(len(buf))+bytes(buf)
            stats=dict(segments=cnt,lo=lo,hi=hi,data=sum(len(d) for _,d in segs),image=hi-lo)
            payload=new
        elif sid==12:
            payload=uleb(1)
        out+=bytes([sid])+uleb(len(payload))+payload
    return bytes(out),stats
def _sl(n):
    o=bytearray()
    while True:
        b=n&0x7f; n>>=7
        if (n==0 and not b&0x40) or (n==-1 and b&0x40): o.append(b); return o
        o.append(b|0x80)
def splice(comp,old,new):
    assert comp[:4]==b'\0asm'
    secs=sections(comp,8); out=bytearray(comp[:8]); hit=0
    for sid,start,pstart,pend in secs:
        payload=comp[pstart:pend]
        if sid==1 and payload==old: payload=new; hit+=1
        out+=bytes([sid])+uleb(len(payload))+payload
    assert hit==1,f"expected exactly one embedded copy, found {hit}"
    return bytes(out)
if __name__=='__main__':
    mode=sys.argv[1]
    if mode=='module':   # densify.py module in.wasm out.wasm
        m=open(sys.argv[2],'rb').read(); n,st=densify_module(m); open(sys.argv[3],'wb').write(n); print("densified:",st,"size",len(m),"->",len(n))
    else:                # densify.py component component.wasm module0.wasm out.wasm
        comp=open(sys.argv[2],'rb').read(); old=open(sys.argv[3],'rb').read(); new,st=densify_module(old)
        open(sys.argv[4],'wb').write(splice(comp,old,new)); print("spliced:",st)
