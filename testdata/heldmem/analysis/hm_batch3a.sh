#!/bin/bash
# known-positive: zero data vs 16 MiB of touched live data held by the guest, hold mode, N in 2,8,32, 2 reps.
for rep in 1 2; do
  for lang in go javascript python csharp; do
    for n in 2 8 32; do
      echo "$(date -u +%H:%M:%S) pos rep=$rep $lang n=$n mb=16"
      bash /tmp/hm5.sh $lang hold $n 0 16 /tmp/hm_pos.jsonl
    done
  done
done
echo POS-DONE
