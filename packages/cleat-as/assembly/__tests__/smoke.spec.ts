/**
 * Smoke tests for @cleat/sdk AssemblyScript exports.
 *
 * Tests pure functions and constants that do not require @external
 * host function imports (no HostCalls instantiation).
 *
 * NOTE: Static method references (e.g. expect(Memory.readString)) cannot
 * be passed as generic arguments to expect() due to a resolver assertion
 * failure in AS 0.27.32. We test methods by calling them instead.
 */
import {
  Memory,
  encodeExportResult,
  decodeExportResult,
  escapeJson,
  OUT_BUF_SIZE,
  SCRATCH_BASE,
  OUTPUT_OFFSET,
  SUSPEND_SENTINEL,
  TERMINAL_ERROR_CODE,
  RETRYABLE_ERROR_CODE,
  ExportDecode,
} from "../index";

describe("Memory layout constants", (): void => {
  it("should have OUT_BUF_SIZE = 65536", (): void => {
    expect<i32>(OUT_BUF_SIZE).toBe(65536);
  });

  it("should have SCRATCH_BASE = 10485760", (): void => {
    expect<usize>(SCRATCH_BASE).toBe(10485760);
  });

  it("should have OUTPUT_OFFSET = 10551296", (): void => {
    expect<usize>(OUTPUT_OFFSET).toBe(10551296);
  });

  it("should have SUSPEND_SENTINEL = 0x4000000000000000", (): void => {
    expect<i64>(SUSPEND_SENTINEL).toBe(0x4000000000000000);
  });

  it("should have TERMINAL_ERROR_CODE = 2", (): void => {
    expect<u32>(TERMINAL_ERROR_CODE).toBe(2);
  });

  it("should have RETRYABLE_ERROR_CODE = 1", (): void => {
    expect<u32>(RETRYABLE_ERROR_CODE).toBe(1);
  });
});

describe("encodeExportResult / decodeExportResult", (): void => {
  it("should roundtrip errCode=0 actualLen=42", (): void => {
    let encoded: i64 = encodeExportResult(0, 42);
    let decoded: ExportDecode = decodeExportResult(encoded);
    expect<u32>(decoded.errCode).toBe(0);
    expect<u32>(decoded.actualLen).toBe(42);
  });

  it("should roundtrip errCode=1 actualLen=0", (): void => {
    let encoded: i64 = encodeExportResult(1, 0);
    let decoded: ExportDecode = decodeExportResult(encoded);
    expect<u32>(decoded.errCode).toBe(1);
    expect<u32>(decoded.actualLen).toBe(0);
  });

  it("should roundtrip errCode=255 actualLen=65535", (): void => {
    let encoded: i64 = encodeExportResult(255, 65535);
    let decoded: ExportDecode = decodeExportResult(encoded);
    expect<u32>(decoded.errCode).toBe(255);
    expect<u32>(decoded.actualLen).toBe(65535);
  });

  it("should preserve errCode=0 actualLen=0", (): void => {
    let encoded: i64 = encodeExportResult(0, 0);
    let decoded: ExportDecode = decodeExportResult(encoded);
    expect<u32>(decoded.errCode).toBe(0);
    expect<u32>(decoded.actualLen).toBe(0);
  });
});

describe("escapeJson", (): void => {
  it("should pass through plain text", (): void => {
    let result: string = escapeJson("hello");
    expect<string>(result).toBe("hello");
  });

  it("should escape double quotes", (): void => {
    let result: string = escapeJson('hello "world"');
    expect<string>(result).toBe('hello \\"world\\"');
  });

  it("should escape backslashes", (): void => {
    let result: string = escapeJson("a\\b");
    expect<string>(result).toBe("a\\\\b");
  });

  it("should escape newlines", (): void => {
    let result: string = escapeJson("line1\nline2");
    expect<string>(result).toBe("line1\\nline2");
  });

  it("should escape tabs", (): void => {
    let result: string = escapeJson("col1\tcol2");
    expect<string>(result).toBe("col1\\tcol2");
  });
});

describe("Memory class", (): void => {
  it("should expose static encodeExportResult that returns correct values", (): void => {
    let encoded: i64 = Memory.encodeExportResult(0, 42);
    let decoded: ExportDecode = decodeExportResult(encoded);
    expect<u32>(decoded.errCode).toBe(0);
    expect<u32>(decoded.actualLen).toBe(42);
  });
});
