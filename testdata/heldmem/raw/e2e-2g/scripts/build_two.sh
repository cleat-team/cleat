export GOFLAGS=-buildvcs=false
cd /basesrc && go build -o /out/slot-before ./cmd/cleat-worker && echo BUILT-BEFORE
cd /src && go build -o /out/slot-after ./cmd/cleat-worker && echo BUILT-AFTER
