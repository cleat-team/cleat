/**
 * JSON builder and parser for AssemblyScript — works with --runtime stub.
 *
 * AssemblyScript 0.27.32 removed the stdlib JSON module. This replacement
 * provides both building and parsing without relying on try/catch or exceptions.
 * All operations use error return values (null / empty string) for runtime stub
 * compatibility.
 *
 * ## Builder Usage
 *
 * ```ts
 * let b = new JsonBuilder();
 * b.startObject();
 * b.addString("name", "Alice");
 * b.addNumber("age", 30);
 * b.startArray("items");
 * b.addRawNumber(42);
 * b.endArray();
 * let json = b.build();  // {"name":"Alice","age":30,"items":[42]}
 * ```
 *
 * ## Parser Usage
 *
 * ```ts
 * let p = new JsonParser();
 * let val = p.parse(input);
 * if (val === null) { /* parse error *\/ }
 * let name = p.getString(val, "name");  // returns "" if missing
 * let items = p.getArray(val, "items");
 * let first = items.length > 0 ? items[0] : null;
 * ```
 */

// ═══════════════════════════════════════════════
// JSON Tokenizer
// ═══════════════════════════════════════════════

const TOK_EOF: i32 = 0;
const TOK_STRING: i32 = 1;
const TOK_NUMBER: i32 = 2;
const TOK_TRUE: i32 = 3;
const TOK_FALSE: i32 = 4;
const TOK_NULL: i32 = 5;
const TOK_LBRACE: i32 = 6;
const TOK_RBRACE: i32 = 7;
const TOK_LBRACKET: i32 = 8;
const TOK_RBRACKET: i32 = 9;
const TOK_COLON: i32 = 10;
const TOK_COMMA: i32 = 11;

class Token {
  type: i32;
  strValue: string;
  numValue: f64;
  constructor(type: i32 = TOK_EOF, strValue: string = "", numValue: f64 = 0.0) {
    this.type = type;
    this.strValue = strValue;
    this.numValue = numValue;
  }
}

class Lexer {
  input: string;
  pos: i32;
  len: i32;

  constructor(input: string) {
    this.input = input;
    this.pos = 0;
    this.len = input.length;
  }

  skipWhitespace(): void {
    while (this.pos < this.len) {
      let c: i32 = this.input.charCodeAt(this.pos);
      if (c == 0x20 || c == 0x09 || c == 0x0a || c == 0x0d) {
        this.pos++;
      } else {
        break;
      }
    }
  }

  peek(): i32 {
    this.skipWhitespace();
    if (this.pos >= this.len) return -1;
    return this.input.charCodeAt(this.pos);
  }

  nextToken(): Token {
    this.skipWhitespace();
    if (this.pos >= this.len) return new Token(TOK_EOF);

    let c: i32 = this.input.charCodeAt(this.pos);
    if (c == 0x22) return this.readString();
    if (c == 0x2d || (c >= 0x30 && c <= 0x39)) return this.readNumber();
    if (c == 0x74) return this.readLiteral("true", TOK_TRUE);
    if (c == 0x66) return this.readLiteral("false", TOK_FALSE);
    if (c == 0x6e) return this.readLiteral("null", TOK_NULL);
    if (c == 0x7b) { this.pos++; return new Token(TOK_LBRACE); }
    if (c == 0x7d) { this.pos++; return new Token(TOK_RBRACE); }
    if (c == 0x5b) { this.pos++; return new Token(TOK_LBRACKET); }
    if (c == 0x5d) { this.pos++; return new Token(TOK_RBRACKET); }
    if (c == 0x3a) { this.pos++; return new Token(TOK_COLON); }
    if (c == 0x2c) { this.pos++; return new Token(TOK_COMMA); }

    // unexpected character — return EOF to signal error
    return new Token(TOK_EOF);
  }

  readString(): Token {
    // skip opening quote
    this.pos++;
    let result: string = "";
    while (this.pos < this.len) {
      let c: i32 = this.input.charCodeAt(this.pos);
      if (c == 0x22) {
        this.pos++;
        return new Token(TOK_STRING, result);
      }
      if (c == 0x5c) {
        // escape sequence
        this.pos++;
        if (this.pos >= this.len) break;
        let esc: i32 = this.input.charCodeAt(this.pos);
        if (esc == 0x22) result += "\"";
        else if (esc == 0x5c) result += "\\";
        else if (esc == 0x2f) result += "/";
        else if (esc == 0x62) result += "\b";
        else if (esc == 0x66) result += "\f";
        else if (esc == 0x6e) result += "\n";
        else if (esc == 0x72) result += "\r";
        else if (esc == 0x74) result += "\t";
        else if (esc == 0x75) {
          // unicode escape \uXXXX
          if (this.pos + 4 < this.len) {
            let hexStr: string = this.input.substring(this.pos + 1, this.pos + 5);
            let codePoint: i32 = 0;
            for (let i: i32 = 0; i < 4; i++) {
              let hc: i32 = hexStr.charCodeAt(i);
              codePoint <<= 4;
              if (hc >= 0x30 && hc <= 0x39) codePoint |= (hc - 0x30);
              else if (hc >= 0x41 && hc <= 0x46) codePoint |= (hc - 0x41 + 10);
              else if (hc >= 0x61 && hc <= 0x66) codePoint |= (hc - 0x61 + 10);
            }
            result += String.fromCharCode(codePoint);
            this.pos += 4;
          }
        } else {
          result += String.fromCharCode(esc);
        }
        this.pos++;
      } else {
        result += String.fromCharCode(c);
        this.pos++;
      }
    }
    // unterminated string — return empty
    return new Token(TOK_STRING, result);
  }

  readNumber(): Token {
    let start: i32 = this.pos;
    if (this.input.charCodeAt(this.pos) == 0x2d) this.pos++;
    while (this.pos < this.len) {
      let c: i32 = this.input.charCodeAt(this.pos);
      if (c >= 0x30 && c <= 0x39) this.pos++;
      else break;
    }
    if (this.pos < this.len && this.input.charCodeAt(this.pos) == 0x2e) {
      this.pos++;
      while (this.pos < this.len) {
        let c: i32 = this.input.charCodeAt(this.pos);
        if (c >= 0x30 && c <= 0x39) this.pos++;
        else break;
      }
    }
    if (this.pos < this.len && (this.input.charCodeAt(this.pos) | 0x20) == 0x65) {
      this.pos++;
      if (this.pos < this.len && (this.input.charCodeAt(this.pos) == 0x2d || this.input.charCodeAt(this.pos) == 0x2b)) this.pos++;
      while (this.pos < this.len) {
        let c: i32 = this.input.charCodeAt(this.pos);
        if (c >= 0x30 && c <= 0x39) this.pos++;
        else break;
      }
    }
    let numStr: string = this.input.substring(start, this.pos);
    return new Token(TOK_NUMBER, "", parseFloat(numStr));
  }

  readLiteral(expected: string, tokenType: i32): Token {
    let end: i32 = this.pos + expected.length;
    if (end <= this.len && this.input.substring(this.pos, end) == expected) {
      this.pos = end;
      return new Token(tokenType);
    }
    return new Token(TOK_EOF);
  }
}

// ═══════════════════════════════════════════════
// JSON Value Types
// ═══════════════════════════════════════════════

const TYPE_NULL: i32 = 0;
const TYPE_BOOL: i32 = 1;
const TYPE_NUMBER: i32 = 2;
const TYPE_STRING: i32 = 3;
const TYPE_ARRAY: i32 = 4;
const TYPE_OBJECT: i32 = 5;

/**
 * A parsed JSON value. Use `type` to discriminate:
 * - TYPE_NULL (0): value is null
 * - TYPE_BOOL (1): read boolVal
 * - TYPE_NUMBER (2): read numVal
 * - TYPE_STRING (3): read strVal
 * - TYPE_ARRAY (4): read arrItems
 * - TYPE_OBJECT (5): read objKeys / objValues
 */
export class JsonVal {
  type: i32;
  boolVal: bool;
  numVal: f64;
  strVal: string;
  arrItems: JsonVal[];
  objKeys: string[];
  objValues: JsonVal[];

  constructor() {
    this.type = TYPE_NULL;
    this.boolVal = false;
    this.numVal = 0.0;
    this.strVal = "";
    this.arrItems = [];
    this.objKeys = [];
    this.objValues = [];
  }
}

// ═══════════════════════════════════════════════
// JsonParser
// ═══════════════════════════════════════════════

/**
 * Parses JSON strings into a tree of JsonVal nodes.
 *
 * All methods return null on parse error instead of throwing, making this
 * compatible with --runtime stub.
 *
 * Basic usage:
 * ```ts
 * let p = new JsonParser();
 * let val = p.parse(input);
 * if (val === null) { /* handle error *\/ }
 * let name = p.getString(val, "name");
 * let items = p.getArray(val, "items");
 * let first = items.length > 0 ? items[0] : null;
 * ```
 */
export class JsonParser {
  lexer: Lexer;
  current: Token;

  constructor() {
    this.lexer = new Lexer("");
    this.current = new Token();
  }

  /**
   * Parse a JSON string into a JsonVal tree.
   * Returns null on parse error.
   */
  parse(input: string): JsonVal | null {
    this.lexer = new Lexer(input);
    // advance to first token
    this.current = this.lexer.nextToken();
    let result = this.parseValue();
    if (result === null) return null;
    // ensure no trailing garbage (consume trailing whitespace)
    // we already consumed all tokens via parseValue recursion
    return result;
  }

  /**
   * Get a string field from a JSON object by key.
   * Returns "" if the key is not found or the value is not a string.
   */
  getString(obj: JsonVal, key: string): string {
    if (obj.type != TYPE_OBJECT) return "";
    for (let i: i32 = 0; i < obj.objKeys.length; i++) {
      if (obj.objKeys[i] == key) {
        let v: JsonVal = obj.objValues[i];
        if (v.type == TYPE_STRING) return v.strVal;
        return "";
      }
    }
    return "";
  }

  /**
   * Get a number field from a JSON object by key.
   * Returns 0.0 if the key is not found or the value is not a number.
   */
  getNumber(obj: JsonVal, key: string): f64 {
    if (obj.type != TYPE_OBJECT) return 0.0;
    for (let i: i32 = 0; i < obj.objKeys.length; i++) {
      if (obj.objKeys[i] == key) {
        let v: JsonVal = obj.objValues[i];
        if (v.type == TYPE_NUMBER) return v.numVal;
        return 0.0;
      }
    }
    return 0.0;
  }

  /**
   * Get an array field from a JSON object by key.
   * Returns an empty array if the key is not found or the value is not an array.
   */
  getArray(obj: JsonVal, key: string): JsonVal[] {
    if (obj.type != TYPE_OBJECT) return [];
    for (let i: i32 = 0; i < obj.objKeys.length; i++) {
      if (obj.objKeys[i] == key) {
        let v: JsonVal = obj.objValues[i];
        if (v.type == TYPE_ARRAY) return v.arrItems;
        return [];
      }
    }
    return [];
  }

  /**
   * Get a boolean field from a JSON object by key.
   * Returns false if the key is not found or the value is not a boolean.
   */
  getBool(obj: JsonVal, key: string): bool {
    if (obj.type != TYPE_OBJECT) return false;
    for (let i: i32 = 0; i < obj.objKeys.length; i++) {
      if (obj.objKeys[i] == key) {
        let v: JsonVal = obj.objValues[i];
        if (v.type == TYPE_BOOL) return v.boolVal;
        return false;
      }
    }
    return false;
  }

  // --- Internal parsing ---

  private parseValue(): JsonVal | null {
    let tok: Token = this.current;
    if (tok.type == TOK_STRING) {
      let val = new JsonVal();
      val.type = TYPE_STRING;
      val.strVal = tok.strValue;
      this.current = this.lexer.nextToken();
      return val;
    }
    if (tok.type == TOK_NUMBER) {
      let val = new JsonVal();
      val.type = TYPE_NUMBER;
      val.numVal = tok.numValue;
      this.current = this.lexer.nextToken();
      return val;
    }
    if (tok.type == TOK_TRUE) {
      let val = new JsonVal();
      val.type = TYPE_BOOL;
      val.boolVal = true;
      this.current = this.lexer.nextToken();
      return val;
    }
    if (tok.type == TOK_FALSE) {
      let val = new JsonVal();
      val.type = TYPE_BOOL;
      val.boolVal = false;
      this.current = this.lexer.nextToken();
      return val;
    }
    if (tok.type == TOK_NULL) {
      this.current = this.lexer.nextToken();
      return new JsonVal(); // TYPE_NULL by default
    }
    if (tok.type == TOK_LBRACE) return this.parseObject();
    if (tok.type == TOK_LBRACKET) return this.parseArray();
    return null; // unexpected token
  }

  private parseObject(): JsonVal | null {
    let obj = new JsonVal();
    obj.type = TYPE_OBJECT;
    this.current = this.lexer.nextToken(); // consume '{'
    if (this.current.type == TOK_RBRACE) {
      this.current = this.lexer.nextToken();
      return obj;
    }
    while (true) {
      // expect string key
      if (this.current.type != TOK_STRING) return null;
      let key: string = this.current.strValue;
      this.current = this.lexer.nextToken();
      // expect colon
      if (this.current.type != TOK_COLON) return null;
      this.current = this.lexer.nextToken();
      // parse value
      let val: JsonVal | null = this.parseValue();
      if (val === null) return null;
      obj.objKeys.push(key);
      obj.objValues.push(val);
      // expect comma or closing brace
      if (this.current.type == TOK_RBRACE) {
        this.current = this.lexer.nextToken();
        return obj;
      }
      if (this.current.type != TOK_COMMA) return null;
      this.current = this.lexer.nextToken();
    }
  }

  private parseArray(): JsonVal | null {
    let arr = new JsonVal();
    arr.type = TYPE_ARRAY;
    this.current = this.lexer.nextToken(); // consume '['
    if (this.current.type == TOK_RBRACKET) {
      this.current = this.lexer.nextToken();
      return arr;
    }
    while (true) {
      let val: JsonVal | null = this.parseValue();
      if (val === null) return null;
      arr.arrItems.push(val);
      if (this.current.type == TOK_RBRACKET) {
        this.current = this.lexer.nextToken();
        return arr;
      }
      if (this.current.type != TOK_COMMA) return null;
      this.current = this.lexer.nextToken();
    }
  }
}

// ═══════════════════════════════════════════════
// JsonBuilder
// ═══════════════════════════════════════════════

/**
 * Builds JSON strings incrementally.
 *
 * Compatible with --runtime stub: all methods return void (no exceptions thrown).
 *
 * Usage:
 * ```ts
 * let b = new JsonBuilder();
 * b.startObject();
 * b.addString("name", "Alice");
 * b.addNumber("age", 30);
 * b.startArray("tags");
 * b.addRawString("admin");
 * b.addRawNumber(1);
 * b.endArray();
 * let json = b.build();
 * // {"name":"Alice","age":30,"tags":["admin",1]}
 * ```
 */
export class JsonBuilder {
  private parts: string[];
  private depth: i32;
  private hasElements: bool[];

  constructor() {
    this.parts = [];
    this.depth = 0;
    this.hasElements = new Array<bool>(16);
    for (let i: i32 = 0; i < 16; i++) this.hasElements[i] = false;
  }

  /**
   * Start a JSON object at the current nesting level.
   * Call endObject() when done adding fields.
   */
  startObject(): void {
    this.pushComma();
    this.emit("{");
    this.depth++;
    if (this.depth >= this.hasElements.length) {
      this.hasElements.push(false);
    } else {
      this.hasElements[this.depth] = false;
    }
  }

  /**
   * Start a JSON object as a field value.
   */
  startObjectField(key: string): void {
    this.pushComma();
    this.emitKey(key);
    this.emit("{");
    this.depth++;
    if (this.depth >= this.hasElements.length) {
      this.hasElements.push(false);
    } else {
      this.hasElements[this.depth] = false;
    }
  }

  /**
   * End the current JSON object.
   */
  endObject(): void {
    this.depth--;
    this.emit("}");
  }

  /**
   * Start a JSON array as a field value.
   */
  startArray(key: string): void {
    this.pushComma();
    this.emitKey(key);
    this.emit("[");
    this.depth++;
    if (this.depth >= this.hasElements.length) {
      this.hasElements.push(false);
    } else {
      this.hasElements[this.depth] = false;
    }
  }

  /**
   * Start a root-level JSON array (no key).
   */
  startRootArray(): void {
    this.pushComma();
    this.emit("[");
    this.depth++;
    if (this.depth >= this.hasElements.length) {
      this.hasElements.push(false);
    } else {
      this.hasElements[this.depth] = false;
    }
  }

  /**
   * End the current JSON array.
   */
  endArray(): void {
    this.depth--;
    this.emit("]");
  }

  /**
   * Add a string field to the current object.
   */
  addString(key: string, val: string): void {
    this.pushComma();
    this.emitKey(key);
    this.emitStringValue(val);
  }

  /**
   * Add a number field to the current object.
   */
  addNumber(key: string, val: f64): void {
    this.pushComma();
    this.emitKey(key);
    this.emitNumberValue(val);
  }

  /**
   * Add a boolean field to the current object.
   */
  addBool(key: string, val: bool): void {
    this.pushComma();
    this.emitKey(key);
    this.emit(val ? "true" : "false");
  }

  /**
   * Add a null field to the current object.
   */
  addNull(key: string): void {
    this.pushComma();
    this.emitKey(key);
    this.emit("null");
  }

  /**
   * Add a raw string value to the current array (no key).
   */
  addRawString(val: string): void {
    this.pushComma();
    this.emitStringValue(val);
  }

  /**
   * Add a raw number value to the current array (no key).
   */
  addRawNumber(val: f64): void {
    this.pushComma();
    this.emitNumberValue(val);
  }

  /**
   * Add a raw boolean value to the current array (no key).
   */
  addRawBool(val: bool): void {
    this.pushComma();
    this.emit(val ? "true" : "false");
  }

  /**
   * Add a raw null value to the current array.
   */
  addRawNull(): void {
    this.pushComma();
    this.emit("null");
  }

  /**
   * Build the final JSON string.
   * Must be called at depth 0 (all objects/arrays closed).
   */
  build(): string {
    let result: string = "";
    for (let i: i32 = 0; i < this.parts.length; i++) {
      result += this.parts[i];
    }
    return result;
  }

  /** Clear the builder state for reuse. */
  reset(): void {
    this.parts = [];
    this.depth = 0;
    for (let i: i32 = 0; i < 16; i++) this.hasElements[i] = false;
  }

  // --- Internal helpers ---

  private emit(s: string): void {
    this.parts.push(s);
  }

  private emitKey(key: string): void {
    this.emit("\"");
    this.emit(this.escapeString(key));
    this.emit("\":");
  }

  private emitStringValue(val: string): void {
    this.emit("\"");
    this.emit(this.escapeString(val));
    this.emit("\"");
  }

  private emitNumberValue(val: f64): void {
    // handle integer values without decimal point
    if (val == Math.floor(val) && isFinite(val)) {
      this.emit(val.toString());
    } else {
      this.emit(val.toString());
    }
  }

  private pushComma(): void {
    if (this.depth > 0 && this.hasElements[this.depth]) {
      this.emit(",");
    }
    if (this.depth > 0) {
      this.hasElements[this.depth] = true;
    }
  }

  private escapeString(s: string): string {
    let result: string = "";
    for (let i: i32 = 0; i < s.length; i++) {
      let c: i32 = s.charCodeAt(i);
      if (c == 0x22) result += "\\\"";
      else if (c == 0x5c) result += "\\\\";
      else if (c == 0x08) result += "\\b";
      else if (c == 0x0c) result += "\\f";
      else if (c == 0x0a) result += "\\n";
      else if (c == 0x0d) result += "\\r";
      else if (c == 0x09) result += "\\t";
      else if (c < 0x20) {
        result += "\\u00";
        result += this.hexChar((c >> 4) & 0x0f);
        result += this.hexChar(c & 0x0f);
      } else {
        result += String.fromCharCode(c);
      }
    }
    return result;
  }

  private hexChar(n: i32): string {
    if (n < 10) return String.fromCharCode(0x30 + n);
    return String.fromCharCode(0x61 + n - 10);
  }
}

// ═══════════════════════════════════════════════
// Standalone helpers
// ═══════════════════════════════════════════════

/**
 * Quick-and-dirty JSON string field extraction for flat JSON objects.
 * Returns "" when the field is not present or is not a quoted string.
 *
 * More efficient than full parsing for simple cases.
 */
export function jsonExtractString(json: string, field: string): string {
  let key: string = "\"" + field + "\":\"";
  let start: i32 = json.indexOf(key);
  if (start < 0) return "";
  start += key.length;
  let end: i32 = start;
  while (end < json.length) {
    let c: i32 = json.charCodeAt(end);
    if (c == 0x22) break;
    if (c == 0x5c) end++; // skip escaped char
    end++;
  }
  let result: string = "";
  let i: i32 = start;
  while (i < end) {
    let c: i32 = json.charCodeAt(i);
    if (c == 0x5c && i + 1 < end) {
      let next: i32 = json.charCodeAt(i + 1);
      if (next == 0x22) result += "\"";
      else if (next == 0x5c) result += "\\";
      else if (next == 0x6e) result += "\n";
      else if (next == 0x72) result += "\r";
      else if (next == 0x74) result += "\t";
      else result += String.fromCharCode(next);
      i += 2;
    } else {
      result += String.fromCharCode(c);
      i++;
    }
  }
  return result;
}

/**
 * Quick-and-dirty JSON number extraction for flat JSON objects.
 * Returns 0.0 when the field is not present or is not a number.
 */
export function jsonExtractNumber(json: string, field: string): f64 {
  let key: string = "\"" + field + "\":";
  let start: i32 = json.indexOf(key);
  if (start < 0) return 0.0;
  start += key.length;
  // skip whitespace
  while (start < json.length) {
    let c: i32 = json.charCodeAt(start);
    if (c == 0x20 || c == 0x09 || c == 0x0a || c == 0x0d) start++;
    else break;
  }
  let end: i32 = start;
  if (end < json.length && (json.charCodeAt(end) == 0x2d)) end++;
  while (end < json.length) {
    let c: i32 = json.charCodeAt(end);
    if ((c >= 0x30 && c <= 0x39) || c == 0x2e || c == 0x65 || c == 0x45 || c == 0x2b || c == 0x2d) end++;
    else break;
  }
  if (end <= start) return 0.0;
  let numStr: string = json.substring(start, end);
  return parseFloat(numStr);
}

/**
 * Quick-and-dirty JSON boolean extraction for flat JSON objects.
 * Returns false when the field is not present or is not a boolean.
 */
export function jsonExtractBool(json: string, field: string): bool {
  let key: string = "\"" + field + "\":";
  let start: i32 = json.indexOf(key);
  if (start < 0) return false;
  start += key.length;
  while (start < json.length) {
    let c: i32 = json.charCodeAt(start);
    if (c == 0x20 || c == 0x09) start++;
    else break;
  }
  if (start + 4 <= json.length && json.substring(start, start + 4) == "true") return true;
  return false;
}
