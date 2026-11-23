export GOFLAGS=-buildvcs=false
cd /src && go build -o /out/slot-new ./cmd/cleat-worker && echo BUILT-NEW
