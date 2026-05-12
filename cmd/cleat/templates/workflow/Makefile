.PHONY: build test deploy clean

build:
	cleat build -o workflow.wasm .

test:
	go test ./...

deploy:
	cleat deploy --db "$(DATABASE_URL)" --name my-workflow workflow.wasm

clean:
	rm -f workflow.wasm
