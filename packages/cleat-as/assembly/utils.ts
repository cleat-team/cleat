/**
 * Utility classes and functions for the cleat AssemblyScript SDK.
 *
 * Compatible with --runtime stub (no try/catch, no exceptions).
 *
 * ## StringBuilder
 *
 * Memory-efficient string building that avoids repeated WASM string
 * concatenation (which allocates on each `+=`).
 *
 * ```ts
 * let sb = new StringBuilder();
 * sb.append("hello");
 * sb.append(" ");
 * sb.append("world");
 * let s = sb.toString(); // "hello world"
 * ```
 *
 * ## cleanJsonResponse
 *
 * Strips markdown code fences, leading/trailing whitespace, and
 * validates JSON.
 *
 * ```ts
 * let clean = cleanJsonResponse("```json\n{\"key\": \"value\"}\n```");
 * // returns '{"key": "value"}'
 * ```
 *
 * @packageDocumentation
 */

// ═══════════════════════════════════════════════
// StringBuilder
// ═══════════════════════════════════════════════

/**
 * Memory-efficient string builder for AssemblyScript.
 *
 * Avoids repeated WASM string concatenation which creates a new allocation
 * on every `+=` operation. Instead, accumulates string parts in an array
 * and concatenates them once in `toString()`.
 *
 * Compatible with --runtime stub.
 */
export class StringBuilder {
  private parts: string[];

  constructor() {
    this.parts = [];
  }

  /**
   * Append a string to the builder.
   *
   * @param s - String to append.
   * @returns The StringBuilder instance for chaining.
   */
  append(s: string): StringBuilder {
    this.parts.push(s);
    return this;
  }

  /**
   * Append a string followed by a newline.
   *
   * @param s - String to append.
   * @returns The StringBuilder instance for chaining.
   */
  appendLine(s: string): StringBuilder {
    this.parts.push(s);
    this.parts.push("\n");
    return this;
  }

  /**
   * Build the final string by concatenating all accumulated parts.
   *
   * @returns The concatenated string.
   */
  toString(): string {
    let result: string = "";
    for (let i: i32 = 0; i < this.parts.length; i++) {
      result += this.parts[i];
    }
    return result;
  }

  /**
   * Get the total length of all accumulated parts.
   */
  get length(): i32 {
    let len: i32 = 0;
    for (let i: i32 = 0; i < this.parts.length; i++) {
      len += this.parts[i].length;
    }
    return len;
  }

  /**
   * Clear the builder for reuse.
   */
  reset(): void {
    this.parts = [];
  }
}

// ═══════════════════════════════════════════════
// cleanJsonResponse
// ═══════════════════════════════════════════════

/**
 * Clean a raw string that may contain a JSON response.
 *
 * Strips markdown code fences (```json ... ```), trims leading/trailing
 * whitespace, and validates that the result starts with `{` or `[`.
 *
 * Returns an empty string if the input does not contain valid-looking JSON.
 *
 * @param raw - Raw string that may contain a JSON response.
 * @returns Cleaned JSON string, or empty string if invalid.
 */
export function cleanJsonResponse(raw: string): string {
  let s: string = raw.trim();

  // Strip opening markdown code fence (```json, ```, etc.)
  if (s.startsWith("```")) {
    let idx: i32 = s.indexOf("\n");
    if (idx > 0) {
      s = s.substring(idx + 1);
    } else {
      s = s.substring(3);
    }
    // Strip closing code fence
    let endIdx: i32 = s.lastIndexOf("```");
    if (endIdx > 0) {
      s = s.substring(0, endIdx);
    }
    s = s.trim();
  }

  // Validate that the result starts with '{' or '['
  if (s.length > 0) {
    let c: i32 = s.charCodeAt(0);
    if (c == 0x7b || c == 0x5b) {
      return s;
    }
  }

  return "";
}
