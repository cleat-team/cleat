/**
 * Tests for JsonParser, JsonBuilder, and Saga classes.
 *
 * These classes can be tested in the AS-Pect WASM environment because they
 * are pure AssemblyScript code with no dependency on @external host imports
 * other than indirect type references.
 *
 * NOTE: Static method references cannot be passed as generic arguments to
 * expect() due to a resolver assertion failure in AS 0.27.32. We test methods
 * by calling them and asserting on the result instead.
 */
import {
  JsonParser,
  JsonVal,
  JsonBuilder,
  Saga,
  HostCalls,
  jsonEscape,
  jsonExtractString,
  jsonExtractNumber,
  jsonExtractBool,
  jsonStrArray,
  StringBuilder,
  cleanJsonResponse,
} from "../index";

// ═══════════════════════════════════════════════════════════════════════════════
// Helper: check that two strings are equal (avoids nullable expect issues)
// ═══════════════════════════════════════════════════════════════════════════════

function expectStr(actual: string, expected: string): void {
  expect<string>(actual).toBe(expected);
}

function expectI32(actual: i32, expected: i32): void {
  expect<i32>(actual).toBe(expected);
}

function expectF64(actual: f64, expected: f64): void {
  expect<f64>(actual).toBe(expected);
}

function expectBool(actual: bool, expected: bool): void {
  expect<bool>(actual).toBe(expected);
}

// ═══════════════════════════════════════════════════════════════════════════════
// JsonParser tests
// ═══════════════════════════════════════════════════════════════════════════════

describe("JsonParser — simple values", (): void => {
  it("should parse a string value", (): void => {
    let p = new JsonParser();
    let val = p.parse('{"name":"Alice"}');
    let notNull: bool = val !== null;
    expectBool(notNull, true);
    if (val !== null) {
      expectStr(p.getString(val, "name"), "Alice");
    }
  });

  it("should parse a number value", (): void => {
    let p = new JsonParser();
    let val = p.parse('{"age":30}');
    let notNull: bool = val !== null;
    expectBool(notNull, true);
    if (val !== null) {
      expectF64(p.getNumber(val, "age"), 30.0);
    }
  });

  it("should parse a boolean true value", (): void => {
    let p = new JsonParser();
    let val = p.parse('{"active":true}');
    let notNull: bool = val !== null;
    expectBool(notNull, true);
    if (val !== null) {
      expectBool(p.getBool(val, "active"), true);
    }
  });

  it("should parse a boolean false value", (): void => {
    let p = new JsonParser();
    let val = p.parse('{"active":false}');
    let notNull: bool = val !== null;
    expectBool(notNull, true);
    if (val !== null) {
      expectBool(p.getBool(val, "active"), false);
    }
  });

  it("should parse a null value", (): void => {
    let p = new JsonParser();
    let val = p.parse('{"data":null}');
    let notNull: bool = val !== null;
    expectBool(notNull, true);
    if (val !== null) {
      // null value is the default -- check type is TYPE_OBJECT
      // and the value for "data" is null
      let dataVal: JsonVal | null = null;
      for (let i: i32 = 0; i < val.objKeys.length; i++) {
        if (val.objKeys[i] == "data") {
          dataVal = val.objValues[i];
        }
      }
      let dataNotNull: bool = dataVal !== null;
      expectBool(dataNotNull, true);
      if (dataVal !== null) {
        // TYPE_NULL = 0
        expectI32(dataVal.type, 0);
      }
    }
  });

  it("should parse an empty object", (): void => {
    let p = new JsonParser();
    let val = p.parse("{}");
    let notNull: bool = val !== null;
    expectBool(notNull, true);
    if (val !== null) {
      expectI32(val.objKeys.length, 0);
    }
  });

  it("should parse a negative number", (): void => {
    let p = new JsonParser();
    let val = p.parse('{"temp":-5}');
    let notNull: bool = val !== null;
    expectBool(notNull, true);
    if (val !== null) {
      expectF64(p.getNumber(val, "temp"), -5.0);
    }
  });

  it("should parse a floating-point number", (): void => {
    let p = new JsonParser();
    let val = p.parse('{"pi":3.14}');
    let notNull: bool = val !== null;
    expectBool(notNull, true);
    if (val !== null) {
      expectF64(p.getNumber(val, "pi"), 3.14);
    }
  });
});

describe("JsonParser — arrays", (): void => {
  it("should parse an empty array", (): void => {
    let p = new JsonParser();
    let val = p.parse('{"items":[]}');
    let notNull: bool = val !== null;
    expectBool(notNull, true);
    if (val !== null) {
      let arr = p.getArray(val, "items");
      expectI32(arr.length, 0);
    }
  });

  it("should parse an array of numbers", (): void => {
    let p = new JsonParser();
    let val = p.parse('{"nums":[1,2,3]}');
    let notNull: bool = val !== null;
    expectBool(notNull, true);
    if (val !== null) {
      let arr = p.getArray(val, "nums");
      expectI32(arr.length, 3);
      if (arr.length >= 1) expectF64(arr[0].numVal, 1.0);
      if (arr.length >= 2) expectF64(arr[1].numVal, 2.0);
      if (arr.length >= 3) expectF64(arr[2].numVal, 3.0);
    }
  });

  it("should parse an array of strings", (): void => {
    let p = new JsonParser();
    let val = p.parse('{"tags":["a","b","c"]}');
    let notNull: bool = val !== null;
    expectBool(notNull, true);
    if (val !== null) {
      let arr = p.getArray(val, "tags");
      expectI32(arr.length, 3);
      if (arr.length >= 1) expectStr(arr[0].strVal, "a");
      if (arr.length >= 2) expectStr(arr[1].strVal, "b");
      if (arr.length >= 3) expectStr(arr[2].strVal, "c");
    }
  });

  it("should parse a top-level array", (): void => {
    let p = new JsonParser();
    let val = p.parse('[1,2,3]');
    let notNull: bool = val !== null;
    expectBool(notNull, true);
    if (val !== null) {
      expectI32(val.type, 4); // TYPE_ARRAY = 4
      expectI32(val.arrItems.length, 3);
    }
  });

  it("should return empty array for missing key", (): void => {
    let p = new JsonParser();
    let val = p.parse('{"a":1}');
    let notNull: bool = val !== null;
    expectBool(notNull, true);
    if (val !== null) {
      let arr = p.getArray(val, "nonexistent");
      expectI32(arr.length, 0);
    }
  });
});

describe("JsonParser — nested objects", (): void => {
  it("should parse a nested object", (): void => {
    let p = new JsonParser();
    let val = p.parse('{"user":{"name":"Alice","age":30}}');
    let notNull: bool = val !== null;
    expectBool(notNull, true);
    if (val !== null) {
      // Access nested field by parsing the inner object
      let userVal: JsonVal | null = null;
      for (let i: i32 = 0; i < val.objKeys.length; i++) {
        if (val.objKeys[i] == "user") {
          userVal = val.objValues[i];
        }
      }
      let userNotNull: bool = userVal !== null;
      expectBool(userNotNull, true);
      if (userVal !== null) {
        expectStr(p.getString(userVal, "name"), "Alice");
        expectF64(p.getNumber(userVal, "age"), 30.0);
      }
    }
  });

  it("should parse a deeply nested structure", (): void => {
    let p = new JsonParser();
    let val = p.parse('{"level1":{"level2":{"value":42}}}');
    let notNull: bool = val !== null;
    expectBool(notNull, true);
    if (val !== null) {
      let l1: JsonVal | null = null;
      for (let i: i32 = 0; i < val.objKeys.length; i++) {
        if (val.objKeys[i] == "level1") {
          l1 = val.objValues[i];
        }
      }
      let l1NotNull: bool = l1 !== null;
      expectBool(l1NotNull, true);
      if (l1 !== null) {
        let l2: JsonVal | null = null;
        for (let j: i32 = 0; j < l1.objKeys.length; j++) {
          if (l1.objKeys[j] == "level2") {
            l2 = l1.objValues[j];
          }
        }
        let l2NotNull: bool = l2 !== null;
        expectBool(l2NotNull, true);
        if (l2 !== null) {
          expectF64(p.getNumber(l2, "value"), 42.0);
        }
      }
    }
  });
});

describe("JsonParser — mixed types", (): void => {
  it("should parse multiple fields of different types", (): void => {
    let p = new JsonParser();
    let json = '{"name":"Alice","age":30,"active":true,"score":99.5,"data":null}';
    let val = p.parse(json);
    let notNull: bool = val !== null;
    expectBool(notNull, true);
    if (val !== null) {
      expectStr(p.getString(val, "name"), "Alice");
      expectF64(p.getNumber(val, "age"), 30.0);
      expectBool(p.getBool(val, "active"), true);
      expectF64(p.getNumber(val, "score"), 99.5);
      // null field: getString returns "" for null
      expectStr(p.getString(val, "data"), "");
    }
  });

  it("should return defaults for missing fields", (): void => {
    let p = new JsonParser();
    let val = p.parse('{"name":"Alice"}');
    let notNull: bool = val !== null;
    expectBool(notNull, true);
    if (val !== null) {
      expectStr(p.getString(val, "missing"), "");
      expectF64(p.getNumber(val, "missing"), 0.0);
      expectBool(p.getBool(val, "missing"), false);
    }
  });
});

describe("JsonParser — escaped strings", (): void => {
  it("should parse JSON with escaped quotes", (): void => {
    let p = new JsonParser();
    let val = p.parse('{"text":"hello \\"world\\""}');
    let notNull: bool = val !== null;
    expectBool(notNull, true);
    if (val !== null) {
      expectStr(p.getString(val, "text"), 'hello "world"');
    }
  });

  it("should parse JSON with escaped backslash", (): void => {
    let p = new JsonParser();
    let val = p.parse('{"path":"a\\\\b"}');
    let notNull: bool = val !== null;
    expectBool(notNull, true);
    if (val !== null) {
      expectStr(p.getString(val, "path"), "a\\b");
    }
  });

  it("should parse JSON with escaped newline", (): void => {
    let p = new JsonParser();
    let val = p.parse('{"msg":"line1\\nline2"}');
    let notNull: bool = val !== null;
    expectBool(notNull, true);
    if (val !== null) {
      expectStr(p.getString(val, "msg"), "line1\nline2");
    }
  });

  it("should parse JSON with unicode escape", (): void => {
    let p = new JsonParser();
    let val = p.parse('{"sym":"\\u0041BC"}');
    let notNull: bool = val !== null;
    expectBool(notNull, true);
    if (val !== null) {
      expectStr(p.getString(val, "sym"), "ABC");
    }
  });
});

// ═══════════════════════════════════════════════════════════════════════════════
// JsonBuilder tests
// ═══════════════════════════════════════════════════════════════════════════════

describe("JsonBuilder — objects", (): void => {
  it("should build an empty object", (): void => {
    let b = new JsonBuilder();
    b.startObject();
    b.endObject();
    expectStr(b.build(), "{}");
  });

  it("should build an object with a string field", (): void => {
    let b = new JsonBuilder();
    b.startObject();
    b.addString("name", "Alice");
    b.endObject();
    expectStr(b.build(), '{"name":"Alice"}');
  });

  it("should build an object with multiple fields", (): void => {
    let b = new JsonBuilder();
    b.startObject();
    b.addString("name", "Alice");
    b.addNumber("age", 30);
    b.addBool("active", true);
    b.addNull("data");
    b.endObject();
    let json = b.build();
    expectBool(json.indexOf('"name"') >= 0, true);
    expectBool(json.indexOf('"Alice"') >= 0, true);
    expectBool(json.indexOf('"age"') >= 0, true);
    expectBool(json.indexOf('30') >= 0, true);
    expectBool(json.indexOf('"active"') >= 0, true);
    expectBool(json.indexOf('true') >= 0, true);
    expectBool(json.indexOf('"data"') >= 0, true);
    expectBool(json.indexOf('null') >= 0, true);
  });

  it("should build an object with a number zero", (): void => {
    let b = new JsonBuilder();
    b.startObject();
    b.addNumber("count", 0);
    b.endObject();
    // AS outputs 0.0 for f64 0 via toString()
    let json = b.build();
    expectBool(json.indexOf('"count":0') >= 0, true);
  });
});

describe("JsonBuilder — arrays", (): void => {
  it("should build an array with raw values", (): void => {
    let b = new JsonBuilder();
    b.startObject();
    b.startArray("items");
    b.addRawString("a");
    b.addRawString("b");
    b.endArray();
    b.endObject();
    expectStr(b.build(), '{"items":["a","b"]}');
  });

  it("should build an array with mixed types", (): void => {
    let b = new JsonBuilder();
    b.startObject();
    b.startArray("mixed");
    b.addRawString("text");
    b.addRawNumber(42);
    b.addRawBool(true);
    b.addRawNull();
    b.endArray();
    b.endObject();
    let json = b.build();
    expectBool(json.indexOf('"mixed"') >= 0, true);
    expectBool(json.indexOf('"text"') >= 0, true);
    expectBool(json.indexOf('42') >= 0, true);
    expectBool(json.indexOf('true') >= 0, true);
    expectBool(json.indexOf('null') >= 0, true);
  });

  it("should build a root-level array", (): void => {
    let b = new JsonBuilder();
    b.startRootArray();
    b.addRawNumber(1);
    b.addRawNumber(2);
    b.endArray();
    let json = b.build();
    // AS outputs 1.0,2.0 for f64 values
    expectBool(json.indexOf("[") >= 0, true);
    expectBool(json.indexOf("]") >= 0, true);
    expectBool(json.indexOf("1") >= 0, true);
    expectBool(json.indexOf("2") >= 0, true);
  });
});

describe("JsonBuilder — escaped strings", (): void => {
  it("should escape double quotes", (): void => {
    let b = new JsonBuilder();
    b.startObject();
    b.addString("text", 'hello "world"');
    b.endObject();
    expectStr(b.build(), '{"text":"hello \\"world\\""}');
  });

  it("should escape backslashes", (): void => {
    let b = new JsonBuilder();
    b.startObject();
    b.addString("path", "a\\b");
    b.endObject();
    expectStr(b.build(), '{"path":"a\\\\b"}');
  });

  it("should escape newlines", (): void => {
    let b = new JsonBuilder();
    b.startObject();
    b.addString("msg", "line1\nline2");
    b.endObject();
    expectStr(b.build(), '{"msg":"line1\\nline2"}');
  });

  it("should escape tabs", (): void => {
    let b = new JsonBuilder();
    b.startObject();
    b.addString("col", "a\tb");
    b.endObject();
    expectStr(b.build(), '{"col":"a\\tb"}');
  });
});

describe("JsonBuilder — nested objects", (): void => {
  it("should build a nested object", (): void => {
    let b = new JsonBuilder();
    b.startObject();
    b.startObjectField("user");
    b.addString("name", "Alice");
    b.addNumber("age", 30);
    b.endObject();
    b.endObject();
    let json = b.build();
    expectBool(json.indexOf('"user"') >= 0, true);
    expectBool(json.indexOf('"name"') >= 0, true);
    expectBool(json.indexOf('"Alice"') >= 0, true);
    expectBool(json.indexOf('"age"') >= 0, true);
    expectBool(json.indexOf('30') >= 0, true);
  });
});

// ═══════════════════════════════════════════════════════════════════════════════
// Roundtrip tests (build JSON, parse it back)
// ═══════════════════════════════════════════════════════════════════════════════

describe("JsonBuilder + JsonParser roundtrip", (): void => {
  it("should roundtrip a simple object", (): void => {
    let b = new JsonBuilder();
    b.startObject();
    b.addString("name", "Bob");
    b.addNumber("score", 100);
    b.addBool("pass", true);
    b.endObject();
    let json = b.build();

    let p = new JsonParser();
    let val = p.parse(json);
    let notNull: bool = val !== null;
    expectBool(notNull, true);
    if (val !== null) {
      expectStr(p.getString(val, "name"), "Bob");
      expectF64(p.getNumber(val, "score"), 100.0);
      expectBool(p.getBool(val, "pass"), true);
    }
  });

  it("should roundtrip an array", (): void => {
    let b = new JsonBuilder();
    b.startRootArray();
    b.addRawString("x");
    b.addRawString("y");
    b.endArray();
    let json = b.build();

    let p = new JsonParser();
    let val = p.parse(json);
    let notNull: bool = val !== null;
    expectBool(notNull, true);
    if (val !== null) {
      expectI32(val.type, 4); // TYPE_ARRAY
      expectI32(val.arrItems.length, 2);
      if (val.arrItems.length >= 1) expectStr(val.arrItems[0].strVal, "x");
      if (val.arrItems.length >= 2) expectStr(val.arrItems[1].strVal, "y");
    }
  });

  it("should roundtrip nested objects", (): void => {
    let b = new JsonBuilder();
    b.startObject();
    b.startObjectField("nested");
    b.addString("inner", "value");
    b.endObject();
    b.endObject();
    let json = b.build();

    let p = new JsonParser();
    let val = p.parse(json);
    let notNull: bool = val !== null;
    expectBool(notNull, true);
    if (val !== null) {
      let inner: JsonVal | null = null;
      for (let i: i32 = 0; i < val.objKeys.length; i++) {
        if (val.objKeys[i] == "nested") inner = val.objValues[i];
      }
      let innerNotNull: bool = inner !== null;
      expectBool(innerNotNull, true);
      if (inner !== null) {
        expectStr(p.getString(inner, "inner"), "value");
      }
    }
  });

  it("should roundtrip escaped strings", (): void => {
    let b = new JsonBuilder();
    b.startObject();
    b.addString("msg", "a\"b\\c\nd");
    b.endObject();
    let json = b.build();

    let p = new JsonParser();
    let val = p.parse(json);
    let notNull: bool = val !== null;
    expectBool(notNull, true);
    if (val !== null) {
      expectStr(p.getString(val, "msg"), "a\"b\\c\nd");
    }
  });
});

// ═══════════════════════════════════════════════════════════════════════════════
// jsonEscape function
// ═══════════════════════════════════════════════════════════════════════════════

describe("jsonEscape", (): void => {
  it("should pass through plain text", (): void => {
    expectStr(jsonEscape("hello"), "hello");
  });

  it("should escape double quotes", (): void => {
    expectStr(jsonEscape('hello "world"'), 'hello \\"world\\"');
  });

  it("should escape backslashes", (): void => {
    expectStr(jsonEscape("a\\b"), "a\\\\b");
  });

  it("should escape newlines", (): void => {
    expectStr(jsonEscape("line1\nline2"), "line1\\nline2");
  });

  it("should escape tabs", (): void => {
    expectStr(jsonEscape("col1\tcol2"), "col1\\tcol2");
  });
});

// ═══════════════════════════════════════════════════════════════════════════════
// jsonExtract helpers (lightweight JSON field extraction)
// ═══════════════════════════════════════════════════════════════════════════════

describe("jsonExtractString", (): void => {
  it("should extract a string field", (): void => {
    expectStr(jsonExtractString('{"name":"Alice"}', "name"), "Alice");
  });

  it("should return empty string for missing field", (): void => {
    expectStr(jsonExtractString('{"a":1}', "b"), "");
  });

  it("should handle empty string value", (): void => {
    expectStr(jsonExtractString('{"name":""}', "name"), "");
  });

  it("should extract string with escaped quotes", (): void => {
    expectStr(jsonExtractString('{"text":"hello \\"world\\""}', "text"), 'hello "world"');
  });
});

describe("jsonExtractNumber", (): void => {
  it("should extract an integer", (): void => {
    expectF64(jsonExtractNumber('{"age":30}', "age"), 30.0);
  });

  it("should extract a float", (): void => {
    expectF64(jsonExtractNumber('{"pi":3.14}', "pi"), 3.14);
  });

  it("should extract a negative number", (): void => {
    expectF64(jsonExtractNumber('{"temp":-5}', "temp"), -5.0);
  });

  it("should return 0 for missing field", (): void => {
    expectF64(jsonExtractNumber('{"a":1}', "b"), 0.0);
  });
});

describe("jsonExtractBool", (): void => {
  it("should extract true", (): void => {
    expectBool(jsonExtractBool('{"active":true}', "active"), true);
  });

  it("should extract false", (): void => {
    expectBool(jsonExtractBool('{"active":false}', "active"), false);
  });

  it("should return false for missing field", (): void => {
    expectBool(jsonExtractBool('{"a":1}', "missing"), false);
  });
});

describe("jsonStrArray", (): void => {
  it("should parse an empty array", (): void => {
    let arr = jsonStrArray("[]");
    expectI32(arr.length, 0);
  });

  it("should parse a string array", (): void => {
    let arr = jsonStrArray('["a","b","c"]');
    expectI32(arr.length, 3);
    if (arr.length >= 1) expectStr(arr[0], "a");
    if (arr.length >= 2) expectStr(arr[1], "b");
    if (arr.length >= 3) expectStr(arr[2], "c");
  });

  it("should handle array with null elements", (): void => {
    let arr = jsonStrArray('["a", null, "b"]');
    expectI32(arr.length, 3);
    if (arr.length >= 2) expectStr(arr[1], ""); // null becomes empty string
  });
});

// ═══════════════════════════════════════════════════════════════════════════════
// StringBuilder tests
// ═══════════════════════════════════════════════════════════════════════════════

describe("StringBuilder", (): void => {
  it("should start empty", (): void => {
    let sb = new StringBuilder();
    expectStr(sb.toString(), "");
  });

  it("should concatenate strings", (): void => {
    let sb = new StringBuilder();
    sb.append("hello").append(" ").append("world");
    expectStr(sb.toString(), "hello world");
  });

  it("should append lines", (): void => {
    let sb = new StringBuilder();
    sb.appendLine("line1");
    sb.appendLine("line2");
    let result = sb.toString();
    // Find the newline positions
    let idx1 = result.indexOf("\n");
    expectBool(idx1 >= 0, true);
    let parts: string[] = result.split("\n");
    expectI32(parts.length, 3); // "line1", "line2", ""
    if (parts.length >= 1) expectStr(parts[0], "line1");
    if (parts.length >= 2) expectStr(parts[1], "line2");
  });

  it("should report correct length", (): void => {
    let sb = new StringBuilder();
    sb.append("abc");
    sb.append("de");
    expectI32(sb.length, 5);
  });

  it("should reset correctly", (): void => {
    let sb = new StringBuilder();
    sb.append("hello");
    sb.reset();
    expectStr(sb.toString(), "");
  });
});

// ═══════════════════════════════════════════════════════════════════════════════
// cleanJsonResponse tests
// ═══════════════════════════════════════════════════════════════════════════════

describe("cleanJsonResponse", (): void => {
  it("should pass through valid JSON", (): void => {
    expectStr(cleanJsonResponse('{"a":1}'), '{"a":1}');
  });

  it("should strip markdown code fences", (): void => {
    expectStr(cleanJsonResponse("```json\n{\"a\":1}\n```"), '{"a":1}');
  });

  it("should strip whitespace", (): void => {
    expectStr(cleanJsonResponse('  {"a":1}  '), '{"a":1}');
  });

  it("should return empty string for non-JSON", (): void => {
    expectStr(cleanJsonResponse("hello world"), "");
  });
});

// ═══════════════════════════════════════════════════════════════════════════════
// Saga tests
//
// NOTE: These tests instantiate HostCalls. The as-pect harness provides stub
// implementations for all @external("env", ...) imports so the module can
// instantiate even without a real runtime environment.
// ═══════════════════════════════════════════════════════════════════════════════

// Top-level saga action/compensation functions for testing.
// Using named functions per AS constraint for function references.

let sagaStepLog: string[] = [];

function resetSagaLog(): void {
  sagaStepLog = [];
}

function sagaStep1(h: HostCalls): string {
  sagaStepLog.push("step1");
  return ""; // success
}

function sagaComp1(h: HostCalls): void {
  sagaStepLog.push("comp1");
}

function sagaStep2(h: HostCalls): string {
  sagaStepLog.push("step2");
  return ""; // success
}

function sagaComp2(h: HostCalls): void {
  sagaStepLog.push("comp2");
}

function sagaStep3(h: HostCalls): string {
  sagaStepLog.push("step3");
  return ""; // success
}

function sagaComp3(h: HostCalls): void {
  sagaStepLog.push("comp3");
}

function sagaStepFail(h: HostCalls): string {
  sagaStepLog.push("step_fail");
  return "something went wrong";
}

function sagaStepNoComp(h: HostCalls): string {
  sagaStepLog.push("step_nocomp");
  return ""; // success
}

function sagaCompNoop(h: HostCalls): void {
  // no-op
}

describe("Saga", (): void => {
  it("should execute all steps successfully", (): void => {
    resetSagaLog();
    let saga = new Saga();
    saga.addStep("step1", sagaStep1, sagaComp1);
    saga.addStep("step2", sagaStep2, sagaComp2);
    let host = new HostCalls();
    let result: string = saga.run(host);
    expectStr(result, "");
    expectI32(sagaStepLog.length, 2);
    if (sagaStepLog.length >= 1) expectStr(sagaStepLog[0], "step1");
    if (sagaStepLog.length >= 2) expectStr(sagaStepLog[1], "step2");
  });

  it("should return error when first step fails", (): void => {
    resetSagaLog();
    let saga = new Saga();
    saga.addStep("fail", sagaStepFail, sagaComp1);
    let host = new HostCalls();
    let result: string = saga.run(host);
    // Should return the error and NOT call compensate (no prior steps)
    expectStr(result, "something went wrong");
    expectI32(sagaStepLog.length, 1);
    expectStr(sagaStepLog[0], "step_fail");
  });

  it("should compensate completed steps when later step fails", (): void => {
    resetSagaLog();
    let saga = new Saga();
    saga.addStep("step1", sagaStep1, sagaComp1);
    saga.addStep("step2", sagaStep2, sagaComp2);
    saga.addStep("step3", sagaStepFail, sagaComp3);
    let host = new HostCalls();
    let result: string = saga.run(host);
    expectStr(result, "something went wrong");
    // Step 1 and 2 completed, then step 3 failed.
    // Compensations should run in reverse order: comp2, comp1
    expectI32(sagaStepLog.length, 5); // step1, step2, step_fail, comp2, comp1
    if (sagaStepLog.length >= 5) {
      expectStr(sagaStepLog[0], "step1");
      expectStr(sagaStepLog[1], "step2");
      expectStr(sagaStepLog[2], "step_fail");
      expectStr(sagaStepLog[3], "comp2");
      expectStr(sagaStepLog[4], "comp1");
    }
  });

  it("should handle null compensation gracefully", (): void => {
    resetSagaLog();
    let saga = new Saga();
    // step1 has null compensation (best-effort, no cleanup needed)
    saga.addStep("step1", sagaStep1, null);
    saga.addStep("step2", sagaStepFail, sagaComp2);
    let host = new HostCalls();
    let result: string = saga.run(host);
    expectStr(result, "something went wrong");
    // step1 completed but has null compensate -> no-op.
    // step2 failed -> its compensate (comp2) does NOT run because
    // compensation only reverts already-completed steps, not the failed one.
    expectI32(sagaStepLog.length, 2);
    if (sagaStepLog.length >= 2) {
      expectStr(sagaStepLog[0], "step1");
      expectStr(sagaStepLog[1], "step_fail");
    }
  });

  it("should compensate in correct reverse order with three steps", (): void => {
    resetSagaLog();
    let saga = new Saga();
    saga.addStep("s1", sagaStep1, sagaComp1);
    saga.addStep("s2", sagaStep2, sagaComp2);
    saga.addStep("s3", sagaStep3, sagaComp3);
    saga.addStep("fail", sagaStepFail, sagaCompNoop);
    let host = new HostCalls();
    let result: string = saga.run(host);
    expectStr(result, "something went wrong");
    // Compensations: comp3, comp2, comp1
    expectI32(sagaStepLog.length, 7); // s1,s2,s3,fail,comp3,comp2,comp1
    if (sagaStepLog.length >= 7) {
      expectStr(sagaStepLog[4], "comp3");
      expectStr(sagaStepLog[5], "comp2");
      expectStr(sagaStepLog[6], "comp1");
    }
  });

  it("should addStep return Saga for chaining", (): void => {
    let saga = new Saga();
    let chained = saga.addStep("s1", sagaStep1, sagaComp1);
    expectBool(chained === saga, true);
  });
});
