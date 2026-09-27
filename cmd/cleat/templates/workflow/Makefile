.PHONY: build test deploy clean

build:
	cleat build -o ./out .

test:
	go test ./...

deploy:
	cleat deploy --name my-workflow ./out/process.wasm

clean:
	rm -rf ./out
