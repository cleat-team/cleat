.PHONY: build test deploy clean

build:
	cleat build -o ./out .

test:
	go test ./...

# --name agent, not --name {{.ProjectName}}: see workflow.go's doc comment.
# The deployed name is a contract every SDK's child-starting call relies on.
deploy:
	cleat deploy --name agent ./out/{{.ProjectName}}.wasm

clean:
	rm -rf ./out
