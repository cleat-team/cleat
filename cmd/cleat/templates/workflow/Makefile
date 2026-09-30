.PHONY: build test deploy clean

build:
	cleat build -o ./out .

test:
	go test ./...

deploy:
	cleat deploy --name {{.ProjectName}} ./out/{{.ProjectName}}.wasm

clean:
	rm -rf ./out
