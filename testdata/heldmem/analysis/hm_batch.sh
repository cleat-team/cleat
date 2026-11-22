#!/bin/bash
# main: zero data, N = 1..64, 3 reps (outer), languages x modes. Each run is a fresh process.
OUT=/tmp/hm_main.jsonl
for rep in 1 2 3; do
  for lang in go javascript python; do
    for mode in hold block; do
      for n in 1 2 4 8 16 32 64; do
        echo "$(date -u +%H:%M:%S) rep=$rep $lang $mode n=$n"
        bash /tmp/hm2.sh $lang $mode $n 0 0 $OUT
      done
    done
  done
done
echo BATCH-MAIN-DONE
