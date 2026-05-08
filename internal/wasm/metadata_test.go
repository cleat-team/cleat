package wasm

import (
	"testing"
)

func TestWriteMetadata_InvalidWasmBytes(t *testing.T) {
	meta := &Metadata{
		WorkflowName:         "test",
		WorkflowVersion:      1,
		ABIVersion:           1,
		MinCompatibleVersion: 1,
	}
	// Too-short bytes should cause writeCustomSection to absorb the error
	// and append the section to the original bytes.
	result, err := WriteMetadata(nil, meta)
	if err != nil {
		t.Fatalf("WriteMetadata with nil bytes: %v", err)
	}
	if len(result) == 0 {
		t.Error("expected non-empty result even with nil input")
	}

	result2, err := WriteMetadata([]byte("short"), meta)
	if err != nil {
		t.Fatalf("WriteMetadata with short bytes: %v", err)
	}
	if len(result2) == 0 {
		t.Error("expected non-empty result even with short input")
	}
}

func TestWriteMetadata_InvalidMeta(t *testing.T) {
	// Valid WASM header: magic (4) + version (4) = 8 bytes.
	wasmHeader := []byte{
		0x00, 0x61, 0x73, 0x6d, // magic
		0x01, 0x00, 0x00, 0x00, // version
	}
	meta := &Metadata{
		WorkflowName:         "test-workflow",
		WorkflowVersion:      1,
		ABIVersion:           1,
		MinCompatibleVersion: 1,
	}
	result, err := WriteMetadata(wasmHeader, meta)
	if err != nil {
		t.Fatalf("WriteMetadata: %v", err)
	}
	if len(result) <= len(wasmHeader) {
		t.Error("expected metadata section appended")
	}

	// Round-trip: read back.
	readMeta, err := ReadMetadata(result)
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if readMeta.WorkflowName != "test-workflow" {
		t.Errorf("expected name test-workflow, got %s", readMeta.WorkflowName)
	}
	if readMeta.WorkflowVersion != 1 {
		t.Errorf("expected version 1, got %d", readMeta.WorkflowVersion)
	}
}

func TestWriteMetadata_ReplaceExistingSection(t *testing.T) {
	wasmHeader := []byte{
		0x00, 0x61, 0x73, 0x6d,
		0x01, 0x00, 0x00, 0x00,
	}
	meta1 := &Metadata{
		WorkflowName:         "v1",
		WorkflowVersion:      1,
		ABIVersion:           1,
		MinCompatibleVersion: 1,
	}
	wasm1, err := WriteMetadata(wasmHeader, meta1)
	if err != nil {
		t.Fatalf("WriteMetadata v1: %v", err)
	}

	meta2 := &Metadata{
		WorkflowName:         "v2",
		WorkflowVersion:      2,
		ABIVersion:           1,
		MinCompatibleVersion: 1,
	}
	wasm2, err := WriteMetadata(wasm1, meta2)
	if err != nil {
		t.Fatalf("WriteMetadata v2: %v", err)
	}

	readMeta, err := ReadMetadata(wasm2)
	if err != nil {
		t.Fatalf("ReadMetadata after replace: %v", err)
	}
	if readMeta.WorkflowName != "v2" {
		t.Errorf("expected v2, got %s", readMeta.WorkflowName)
	}
}

func TestStripCustomSection_NotFound(t *testing.T) {
	wasmHeader := []byte{
		0x00, 0x61, 0x73, 0x6d,
		0x01, 0x00, 0x00, 0x00,
	}
	_, err := stripCustomSection(wasmHeader, "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent section")
	}
}

func TestValidate_Errors(t *testing.T) {
	tests := []struct {
		name  string
		meta  Metadata
		errMsg string
	}{
		{"empty name", Metadata{}, "workflow_name is empty"},
		{"zero version", Metadata{WorkflowName: "x"}, "workflow_version must be positive"},
		{"zero abi", Metadata{WorkflowName: "x", WorkflowVersion: 1}, "abi_version must be positive"},
		{"zero min", Metadata{WorkflowName: "x", WorkflowVersion: 1, ABIVersion: 1}, "min_compatible_version must be positive"},
		{"min > abi", Metadata{WorkflowName: "x", WorkflowVersion: 1, ABIVersion: 1, MinCompatibleVersion: 2}, "min_compatible_version (2) exceeds abi_version (1)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.meta.Validate()
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestReadMetadata_InvalidWasm(t *testing.T) {
	_, err := ReadMetadata([]byte{0, 1, 2})
	if err == nil {
		t.Error("expected error for invalid wasm bytes")
	}
	_, err = ReadMetadata(nil)
	if err == nil {
		t.Error("expected error for nil wasm bytes")
	}
}

func TestDecodeULEB128_Truncated(t *testing.T) {
	_, n := decodeULEB128(nil)
	if n != 0 {
		t.Errorf("expected 0 for nil input, got %d", n)
	}
	_, n = decodeULEB128([]byte{0x80})
	if n != 0 {
		t.Errorf("expected 0 for truncated input, got %d", n)
	}
}
