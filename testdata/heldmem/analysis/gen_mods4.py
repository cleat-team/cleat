import sys,os,json,subprocess
exec(open('/tmp/gen_mods.py').read().split("random.seed(1)")[0])
fill=lambda n:bytes([0x41+(i%23) for i in range(min(n,4096))])*(n//4096+1)
def pat(n): return (fill(n))[:n]
os.makedirs('/tmp/wasm/t4',exist_ok=True)
def img(name,mod):
    f=f'/tmp/wasm/t4/{name}.wasm'; open(f,'wb').write(mod); out='/tmp/t4_out.jsonl'
    if os.path.exists(out): os.remove(out)
    env=dict(os.environ,GOFLAGS='-buildvcs=false',HELDMEM_IMAGE_WASM=f,HELDMEM_N='4',HELDMEM_IMAGE_OUT=out)
    subprocess.run(['/tmp/engine9.test','-test.run','^TestMeasureStandaloneImage$','-test.count=1'],cwd='/src/engine',env=env,capture_output=True)
    r=json.loads(open(out).readline()); ok=r['held']['MemfdMaps']-r['base']['MemfdMaps']>=4
    print(f"{'IMAGE   ' if ok else 'NO IMAGE'}  {name}")
MiB=2**20
img('only_one_high_28KB_at_22.44MiB',module(363,[(23532812,pat(28165))]))
img('two_tiny_1KiB_and_22.44MiB',module(363,[(1024,pat(1000)),(23532812,pat(1000))]))
for off_mib in (2,4,8,12,16,20,22):
    img(f'two_tiny_1KiB_and_{off_mib}MiB',module(363,[(1024,pat(1000)),(off_mib*MiB,pat(1000))]))
# the same 22.44 MiB tiny high segment but a bigger memory (so it is not near the end)
img('two_tiny_1KiB_and_22.44MiB_memory_1000pages',module(1000,[(1024,pat(1000)),(23532812,pat(1000))]))
# gap-free dense data up to 22.44MiB then the high segment
img('dense_0_to_22.44MiB_plus_high_seg',module(363,[(0,pat(23532812)),(23532812,pat(28165))]))
