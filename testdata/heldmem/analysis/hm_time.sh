#!/bin/bash
# quiet machine: time to reach N held, N=1 and N=32 (and 64 for the cheap ones), 3 reps, hold mode, zero data
rm -f /tmp/hm_time.jsonl
for rep in 1 2 3; do
  for lang in go javascript python csharp; do
    for n in 1 32; do bash /tmp/hm8.sh $lang hold $n 0 0 /tmp/hm_time.jsonl; done
  done
done
echo TIME-DONE
