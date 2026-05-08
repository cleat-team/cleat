.PHONY: build validate clean generate-sdk all

# Build the WASM binary from the Go source.
build:
	GOOS=wasip1 GOARCH=wasm go build -o plugin.wasm .

# Validate the manifest file.
validate:
	cleat plugin validate --manifest plugin.json

# Generate SDK wrappers from the manifest.
generate-sdk:
	cleat-gen-plugin --manifest plugin.json --lang typescript --out generated/plugins.ts
	cleat-gen-plugin --manifest plugin.json --lang python --out generated/plugins.py

# Remove build artifacts.
clean:
	rm -f plugin.wasm
	rm -rf generated/

# Default target: build and validate.
all: validate build generate-sdk
