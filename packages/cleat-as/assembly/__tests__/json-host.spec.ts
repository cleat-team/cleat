/**
 * Tests for the JSON class that delegates to cleat_json_parse and
 * cleat_json_stringify host imports.
 *
 * These tests exercise the HostCalls bridge to the host runtime's
 * encoding/json library. The as-pect harness provides JS stub
 * implementations for the @external("env", ...) imports that use
 * JavaScript's native JSON.parse/JSON.stringify.
 *
 * NOTE: The JSON class (unlike JsonParser/JsonBuilder) does NOT
 * return a parsed value tree. It validates the input JSON via the
 * host and returns a normalized JSON string, or null on failure.
 */
import {
  JSON,
} from "../index";

// ═══════════════════════════════════════════════════════════════════════════════
// Helpers
// ═══════════════════════════════════════════════════════════════════════════════

function expectStr(actual: string, expected: string): void {
  expect<string>(actual).toBe(expected);
}

function expectNull(val: string | null): void {
  expect<bool>(val === null).toBe(true);
}

function expectNotNull(val: string | null): void {
  expect<bool>(val !== null).toBe(true);
}

// ═══════════════════════════════════════════════════════════════════════════════
// JSON.parse tests
// ═══════════════════════════════════════════════════════════════════════════════

describe("JSON.parse", (): void => {
  it("should parse a simple object", (): void => {
    let result: string | null = JSON.parse('{"a":1}');
    expectNotNull(result);
    if (result !== null) {
      expectStr(result, '{"a":1}');
    }
  });

  it("should parse a string value", (): void => {
    let result: string | null = JSON.parse('{"name":"Alice"}');
    expectNotNull(result);
    if (result !== null) {
      // The host normalizes JSON, so the result should be valid
      expect<bool>(result.indexOf('"Alice"') >= 0).toBe(true);
    }
  });

  it("should parse a nested object", (): void => {
    let result: string | null = JSON.parse('{"outer":{"inner":42}}');
    expectNotNull(result);
    if (result !== null) {
      expect<bool>(result.indexOf('"outer"') >= 0).toBe(true);
      expect<bool>(result.indexOf('"inner"') >= 0).toBe(true);
      expect<bool>(result.indexOf('42') >= 0).toBe(true);
    }
  });

  it("should parse a JSON array", (): void => {
    let result: string | null = JSON.parse('[1,2,3]');
    expectNotNull(result);
    if (result !== null) {
      expect<bool>(result.indexOf('1') >= 0).toBe(true);
      expect<bool>(result.indexOf('2') >= 0).toBe(true);
      expect<bool>(result.indexOf('3') >= 0).toBe(true);
    }
  });

  it("should handle boolean and null values", (): void => {
    let result: string | null = JSON.parse('{"b":true,"n":null}');
    expectNotNull(result);
    if (result !== null) {
      expect<bool>(result.indexOf('true') >= 0).toBe(true);
      expect<bool>(result.indexOf('null') >= 0).toBe(true);
    }
  });

  it("should normalize JSON (remove extra whitespace)", (): void => {
    let result: string | null = JSON.parse('{  "a"  :  1  }');
    expectNotNull(result);
    if (result !== null) {
      // Normalized: no extra whitespace
      expectStr(result, '{"a":1}');
    }
  });

  it("should return null for invalid JSON", (): void => {
    let result: string | null = JSON.parse('{invalid}');
    expectNull(result);
  });

  it("should return null for truncated JSON", (): void => {
    let result: string | null = JSON.parse('{"a":');
    expectNull(result);
  });

  it("should return null for single token", (): void => {
    let result: string | null = JSON.parse('hello');
    expectNull(result);
  });
});

// ═══════════════════════════════════════════════════════════════════════════════
// JSON.stringify tests
// ═══════════════════════════════════════════════════════════════════════════════

describe("JSON.stringify", (): void => {
  it("should serialize a simple object", (): void => {
    let result: string | null = JSON.stringify('{"a":1}');
    expectNotNull(result);
    if (result !== null) {
      expectStr(result, '{"a":1}');
    }
  });

  it("should serialize a nested structure", (): void => {
    let result: string | null = JSON.stringify('{"x":{"y":[1,2]}}');
    expectNotNull(result);
    if (result !== null) {
      expect<bool>(result.indexOf('"x"') >= 0).toBe(true);
      expect<bool>(result.indexOf('"y"') >= 0).toBe(true);
      expect<bool>(result.indexOf('1') >= 0).toBe(true);
    }
  });

  it("should serialize an array", (): void => {
    let result: string | null = JSON.stringify('["a","b","c"]');
    expectNotNull(result);
    if (result !== null) {
      expectStr(result, '["a","b","c"]');
    }
  });

  it("should normalize JSON (remove whitespace)", (): void => {
    let result: string | null = JSON.stringify('{"a" : 1 , "b" : 2}');
    expectNotNull(result);
    if (result !== null) {
      expectStr(result, '{"a":1,"b":2}');
    }
  });

  it("should return null for invalid JSON", (): void => {
    let result: string | null = JSON.stringify('{bad}');
    expectNull(result);
  });

  it("should return null for unparseable input", (): void => {
    let result: string | null = JSON.stringify('not json at all');
    expectNull(result);
  });
});

// ═══════════════════════════════════════════════════════════════════════════════
// Roundtrip tests
// ═══════════════════════════════════════════════════════════════════════════════

describe("JSON roundtrip", (): void => {
  it("should roundtrip a simple object", (): void => {
    let input: string = '{"name":"Bob","age":30}';
    let parsed: string | null = JSON.parse(input);
    expectNotNull(parsed);
    if (parsed !== null) {
      let stringified: string | null = JSON.stringify(parsed);
      expectNotNull(stringified);
      if (stringified !== null) {
        expectStr(stringified, '{"name":"Bob","age":30}');
      }
    }
  });

  it("should roundtrip an array", (): void => {
    let input: string = '["x","y","z"]';
    let parsed: string | null = JSON.parse(input);
    expectNotNull(parsed);
    if (parsed !== null) {
      let stringified: string | null = JSON.stringify(parsed);
      expectNotNull(stringified);
      if (stringified !== null) {
        expectStr(stringified, '["x","y","z"]');
      }
    }
  });

  it("should roundtrip a complex nested structure", (): void => {
    let input: string = '{"level1":{"level2":{"value":99,"active":true}}}';
    let parsed: string | null = JSON.parse(input);
    expectNotNull(parsed);
    if (parsed !== null) {
      let stringified: string | null = JSON.stringify(parsed);
      expectNotNull(stringified);
      if (stringified !== null) {
        expect<bool>(stringified.indexOf('"value"') >= 0).toBe(true);
        expect<bool>(stringified.indexOf('99') >= 0).toBe(true);
        expect<bool>(stringified.indexOf('true') >= 0).toBe(true);
      }
    }
  });

  it("should roundtrip with normalized whitespace", (): void => {
    // Input with extra whitespace
    let input: string = '{  "key"  :  "val"  }';
    let parsed: string | null = JSON.parse(input);
    expectNotNull(parsed);
    if (parsed !== null) {
      // After parse, whitespace is normalized
      let stringified: string | null = JSON.stringify(parsed);
      expectNotNull(stringified);
      if (stringified !== null) {
        // Both parse and stringify produce the same normalized output
        expectStr(parsed, stringified);
      }
    }
  });
});
