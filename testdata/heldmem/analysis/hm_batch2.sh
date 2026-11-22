#!/bin/bash
# C# matrix (hold + block), N up to 64, then the copy-on-write experiment (hold, zero data), each run a fresh process.
for rep in 1 2 3; do
  for mode in hold block; do
    for n in 1 2 4 8 16 32 64; do echo "$(date -u +%H:%M:%S) cs rep=$rep $mode n=$n"; bash /tmp/hm3.sh csharp $mode $n 0 0 /tmp/hm_cs.jsonl; done
  done
done
echo CS-DONE
for rep in 1 2 3; do
  for lang in go javascript python csharp; do
    for cow in 0 1; do
      for n in 1 4 16 32; do
        echo "$(date -u +%H:%M:%S) cow=$cow rep=$rep $lang n=$n"
        HELDMEM_COW=$cow bash /tmp/hm3.sh $lang hold $n 0 0 /tmp/hm_cow$cow.jsonl
      done
    done
  done
done
echo BATCH2-DONE
