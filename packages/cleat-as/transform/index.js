"use strict";

const fs = require("fs");
const path = require("path");

/**
 * AssemblyScript transformer for the cleat durable execution framework.
 *
 * Generates WASM export wrappers for functions decorated with @durableEntry.
 * The wrappers conform to the cleat ABI:
 *   (argsPtr: usize, argsLen: i32, outPtr: usize, maxOutLen: i32) => i64
 *
 * For a user function like:
 *
 *   @durableEntry
 *   function placeOrder(h: HostCalls, input: PlaceOrderInput): string { ... }
 *
 * The transformer:
 *   1. Renames the original function to __durable_inner_placeOrder
 *   2. Strips the @durableEntry decorator
 *   3. Generates an export wrapper (with the original name) in the same source
 *
 * Usage in asconfig.json:
 *   { "options": { "transform": ["@cleat/transform"] } }
 *
 * The generated wrapper:
 *   - Reads input JSON from WASM linear memory at (argsPtr, argsLen)
 *   - Deserializes JSON into the user's input type via JSON.parse<T>
 *   - Creates a HostCalls instance from @cleat/sdk
 *   - Invokes the renamed inner function
 *   - Serializes the result to JSON and writes it to (outPtr, maxOutLen)
 *   - Returns a packed i64: low 32 bits = errCode, high 32 bits = actualLen
 *   - On error: writes {"error":"..."} to the output buffer and returns errCode=1
 */
class CleatEntryTransformer {

  // ---------------------------------------------------------------
  // AssemblyScript transformer hook - called after all sources are parsed
  // ---------------------------------------------------------------
  afterParse(parser) {
    const program = parser.program;
    if (!program || !program.sources) return;

    // Phase 1: Find @durableEntry functions grouped by source file
    const sourceEntries = this._findDurableEntries(program);
    if (sourceEntries.length === 0) return;

    // Phase 2: Modify AST - rename functions and strip decorators
    this._renameEntries(sourceEntries);

    // Phase 3: Generate wrapper source code and inject into the source
    this._injectWrappers(parser, sourceEntries);
  }

  // ---------------------------------------------------------------
  // Phase 1: Walk all sources and collect @durableEntry metadata
  // ---------------------------------------------------------------
  _findDurableEntries(program) {
    const sourceEntries = [];

    for (const source of program.sources) {
      if (!source || !source.statements) continue;

      const entries = [];
      for (const stmt of source.statements) {
        if (!this._isDurableEntryFunc(stmt)) continue;

        // Validate: must have at least one parameter (HostCalls)
        const params = stmt.signature.parameters || [];
        if (params.length === 0) {
          console.error(
            "[@cleat/transform] Warning: @durableEntry function '" +
            (stmt.name ? stmt.name.text : "unknown") +
            "' has no parameters. The first parameter must be HostCalls."
          );
          continue;
        }

        const info = this._extractEntryInfo(stmt);
        entries.push(info);
      }

      if (entries.length > 0) {
        sourceEntries.push({ source, entries });
      }
    }

    return sourceEntries;
  }

  // ---------------------------------------------------------------
  // Check if a statement is a function declaration with @durableEntry
  // ---------------------------------------------------------------
  _isDurableEntryFunc(stmt) {
    if (!stmt || typeof stmt !== "object") return false;
    // Must look like a function declaration with decorators
    if (!stmt.name || !stmt.signature || !stmt.decorators) return false;
    if (!Array.isArray(stmt.decorators) || stmt.decorators.length === 0) return false;
    // One of the decorators must be @durableEntry
    return stmt.decorators.some(
      d => d && d.name && d.name.text === "durableEntry"
    );
  }

  // ---------------------------------------------------------------
  // Extract metadata from a @durableEntry function declaration
  // ---------------------------------------------------------------
  _extractEntryInfo(stmt) {
    const funcName = stmt.name.text;
    const innerName = `__durable_inner_${funcName}`;

    // Extract parameters skipping the first one (h: HostCalls)
    const params = stmt.signature.parameters || [];
    const userParams = params.slice(1);

    const paramNames = [];
    const paramTypes = [];
    const callArgs = ["h"];

    for (const p of userParams) {
      const pName = p.name && p.name.text ? p.name.text : "_";
      const pType = p.type && p.type.text ? p.type.text : "string";

      paramNames.push(pName);
      paramTypes.push(pType);
      callArgs.push(pName);
    }

    // Return type string
    const retTypeNode = stmt.signature.returnType;
    const retTypeStr = retTypeNode ? retTypeNode.text : "void";
    const isVoid = retTypeStr === "void";
    const isString = retTypeStr === "string" || retTypeStr === "String";

    return {
      funcName,
      innerName,
      paramNames,
      paramTypes,
      callArgs,
      retTypeStr,
      isVoid,
      isString,
    };
  }

  // ---------------------------------------------------------------
  // Phase 2: Rename @durableEntry functions and strip decorators
  // ---------------------------------------------------------------
  _renameEntries(sourceEntries) {
    for (const { source, entries } of sourceEntries) {
      for (const stmt of source.statements) {
        if (!this._isDurableEntryFunc(stmt)) continue;

        const entry = entries.find(
          e => stmt.name && stmt.name.text === e.funcName
        );
        if (!entry) continue;

        // Rename the function so the generated wrapper can use the original name
        stmt.name.text = entry.innerName;

        // Remove @durableEntry decorator
        stmt.decorators = stmt.decorators.filter(
          d => !(d.name && d.name.text === "durableEntry")
        );
      }
    }
  }

  // ---------------------------------------------------------------
  // Phase 3: Generate wrapper code and inject it into source files
  // ---------------------------------------------------------------
  _injectWrappers(parser, sourceEntries) {
    for (const { source, entries } of sourceEntries) {
      const needsImport = !this._hasCleatSdkImport(source);
      const wrapperCode = this._generateWrappers(entries);

      try {
        // Insert @cleat/sdk import at the TOP of the source if needed
        if (needsImport) {
          const importSrc = parser.parseFile(
            "~lib/generated/cleat-import.ts",
            'import { HostCalls, Memory, SUSPEND_SENTINEL } from "@cleat/sdk";\n',
            false
          );
          if (importSrc && importSrc.statements) {
            // unshift in reverse order so they appear in original order
            for (let i = importSrc.statements.length - 1; i >= 0; i--) {
              const s = importSrc.statements[i];
              if (s) source.statements.unshift(s);
            }
          }
        }

        // Append wrapper functions at the END of the source
        const wrapSrc = parser.parseFile(
          "~lib/generated/cleat-wrappers.ts",
          wrapperCode,
          false
        );
        if (wrapSrc && wrapSrc.statements) {
          for (const s of wrapSrc.statements) {
            if (s) source.statements.push(s);
          }
        }
      } catch (_e) {
        this._writeFallback(wrapperCode);
      }
    }
  }

  // ---------------------------------------------------------------
  // Check if a source already imports from @cleat/sdk
  // ---------------------------------------------------------------
  _hasCleatSdkImport(source) {
    if (!source.statements) return false;

    for (const stmt of source.statements) {
      if (!stmt) continue;

      // Probe various property paths used by different AS versions
      const modName =
        stmt.moduleName ||
        (stmt.internalNamespace && stmt.internalNamespace.text) ||
        (stmt.from && (typeof stmt.from === "string" ? stmt.from : stmt.from.text)) ||
        (stmt.module && (typeof stmt.module === "string" ? stmt.module : stmt.module.text));

      if (modName === "@cleat/sdk") return true;

      if (stmt.namespace && stmt.namespace.text === "@cleat/sdk") return true;
    }

    return false;
  }

  // ---------------------------------------------------------------
  // Fallback: write wrapper code to a file on disk
  // ---------------------------------------------------------------
  _writeFallback(code) {
    try {
      const cwd = process.cwd();
      const outDir = path.join(cwd, "assembly", "generated");
      if (!fs.existsSync(outDir)) {
        fs.mkdirSync(outDir, { recursive: true });
      }
      const outPath = path.join(outDir, "cleat-wrappers.ts");
      fs.writeFileSync(outPath, code, "utf-8");
      console.error(
        "[@cleat/transform] Wrote wrapper file to " + outPath +
        ". Add it to your asconfig.json entries."
      );
    } catch (_e) {
      console.error("[@cleat/transform] Could not write fallback wrapper file.");
    }
  }

  // ---------------------------------------------------------------
  // Generate wrapper export function for a list of entry functions
  // ---------------------------------------------------------------
  _generateWrappers(entries) {
    let code = "";
    code += "// ---- Generated cleat ABI wrappers ----\n";
    code += "// Auto-generated by @cleat/transform. Do not edit.\n";
    code += "// Each wrapper is a WASM export conforming to the cleat ABI.\n";
    code += "// Input types must use @serializable or extend JSON.Serializable.\n\n";

    for (const entry of entries) {
      code += this._generateSingleWrapper(entry);
    }

    return code;
  }

  // ---------------------------------------------------------------
  // Generate a single wrapper export function
  // ---------------------------------------------------------------
  _generateSingleWrapper(info) {
    const {
      funcName,
      innerName,
      paramNames,
      paramTypes,
      callArgs,
      retTypeStr,
      isVoid,
      isString,
    } = info;

    let code = "";

    // ABI export signature: (argsPtr, argsLen, outPtr, maxOutLen) => i64
    // argsPtr/argsLen: pointer and length of the input JSON in linear memory
    // outPtr/maxOutLen: output buffer where the result JSON will be written
    // return value: packed i64 (low 32 = errCode, high 32 = actualLen)
    code += `export function ${funcName}(\n`;
    code += `  argsPtr: usize,\n`;
    code += `  argsLen: i32,\n`;
    code += `  outPtr: usize,\n`;
    code += `  maxOutLen: i32\n`;
    code += `): i64 {\n`;

    // ------------------------------------------------------------------
    // Step 1: Read input JSON string from WASM linear memory
    // ------------------------------------------------------------------
    code += `  // ---- Step 1: Read input from linear memory ----\n`;
    code += `  let argsJson: string = "";\n`;
    code += `  if (argsLen > 0) {\n`;
    code += `    argsJson = Memory.readString(argsPtr, argsLen);\n`;
    code += `  }\n\n`;

    // ------------------------------------------------------------------
    // Step 2: Deserialize JSON into the input parameter(s)
    // ------------------------------------------------------------------
    if (paramNames.length === 0) {
      // No input parameters after HostCalls — nothing to deserialize
      // Keep this section empty for clarity
      code += `  // ---- Step 2: No input parameters to deserialize ----\n\n`;
    } else if (paramNames.length === 1) {
      // Single input parameter: deserialize the entire JSON into this type
      // The user's type must be @serializable for JSON.parse<T>() to work.
      const pname = paramNames[0];
      const ptype = paramTypes[0];
      code += `  // ---- Step 2: Deserialize JSON into ${ptype} ----\n`;
      code += `  let ${pname}: ${ptype} = JSON.parse<${ptype}>(argsJson);\n\n`;
    } else {
      // Multiple input parameters: parse the JSON as an object and extract
      // each field individually. Each field name must match a JSON key.
      code += `  // ---- Step 2: Deserialize multiple parameters from JSON ----\n`;
      code += `  let _parsed: JSON.Obj = <JSON.Obj>JSON.parse(argsJson);\n`;
      for (let i = 0; i < paramNames.length; i++) {
        const pname = paramNames[i];
        const ptype = paramTypes[i];
        code += this._getDeserializeCode(pname, ptype);
      }
      code += `\n`;
    }

    // ------------------------------------------------------------------
    // Step 3: Create HostCalls instance and invoke the workflow function
    // ------------------------------------------------------------------
    code += `  // ---- Step 3: Invoke the workflow function ----\n`;
    code += `  const h = new HostCalls();\n\n`;

    const callExpr = `${innerName}(${callArgs.join(", ")})`;

    if (isVoid) {
      code += `    ${callExpr};\n`;
      code += `\n`;
      code += `    // ---- Step 4: Write success response ----\n`;
      code += `    const _out = '{"ok":true}';\n`;
      code += `    const _written = Memory.writeString(outPtr, maxOutLen, _out);\n`;
      code += `    return Memory.encodeExportResult(0, _written);\n`;
    } else if (isString) {
      // String return types are wrapped in JSON string quotes.
      // The output is a valid JSON string value: "the_result".
      code += `    const _result: string = ${callExpr};\n`;
      code += `\n`;
      code += `    // Check for suspend sentinel ----\n`;
      code += `    if (changetype<u64>(_result) == SUSPEND_SENTINEL) {\n`;
      code += `        return SUSPEND_SENTINEL;\n`;
      code += `    }\n`;
      code += `\n`;
      code += `    // ---- Step 4: Write result to output buffer ----\n`;
      code += `    const _out = JSON.stringify(_result);\n`;
      code += `    const _written = Memory.writeString(outPtr, maxOutLen, _out);\n`;
      code += `    return Memory.encodeExportResult(0, _written);\n`;
    } else {
      // Object/other return: serialize via JSON.stringify.
      // The return type must be JSON-serializable (extends JSON.Value or
      // uses @serializable).
      code += `    const _result = ${callExpr};\n`;
      code += `\n`;
      code += `    // Check for suspend sentinel ----\n`;
      code += `    if (changetype<u64>(_result) == SUSPEND_SENTINEL) {\n`;
      code += `        return SUSPEND_SENTINEL;\n`;
      code += `    }\n`;
      code += `\n`;
      code += `    // ---- Step 4: Serialize result to JSON and write to memory ----\n`;
      code += `    const _out = JSON.stringify(_result);\n`;
      code += `    const _written = Memory.writeString(outPtr, maxOutLen, _out);\n`;
      code += `    return Memory.encodeExportResult(0, _written);\n`;
    }


    code += `}\n\n`;
    return code;
  }

  // ---------------------------------------------------------------
  // Generate a standard error return block for try/catch
  // ---------------------------------------------------------------
  _makeErrorReturn(errorMessage) {
    return (
      `    const _errBody = '{"error":"${errorMessage}"}';\n` +
      `    const _errWritten = Memory.writeString(outPtr, maxOutLen, _errBody);\n` +
      `    return Memory.encodeExportResult(1, _errWritten);\n`
    );
  }

  // ---------------------------------------------------------------
  // Generate deserialization code for a single parameter based on type
  // Used by the multi-parameter branch to select the correct JSON getter
  // ---------------------------------------------------------------
  _getDeserializeCode(pname, ptype) {
    // Map AS types to the correct JSON getter from the AS JSON library
    if (ptype === "string" || ptype === "String") {
      return `  let ${pname}: ${ptype} = _parsed.getString("${pname}") as ${ptype};\n`;
    } else if (ptype === "i32" || ptype === "u32" || ptype === "i64" || ptype === "u64") {
      return `  let ${pname}: ${ptype} = _parsed.getInteger("${pname}") as ${ptype};\n`;
    } else if (ptype === "f64" || ptype === "f32") {
      return `  let ${pname}: ${ptype} = _parsed.getFloat("${pname}") as ${ptype};\n`;
    } else if (ptype === "bool" || ptype === "boolean") {
      return `  let ${pname}: ${ptype} = _parsed.getBool("${pname}");\n`;
    } else {
      // Unknown type — throw a compile-time error from the transformer
      throw new Error(
        "[@cleat/transform] Unsupported type '" + ptype +
        "' for multi-parameter entry function parameter '" + pname +
        "'. Supported types: string, i32, u32, i64, u64, f64, f32, bool"
      );
    }
  }
}

// -----------------------------------------------------------------
// Module exports for the AssemblyScript compiler transformer API.
// The AS compiler instantiates this class and calls afterParse().
// -----------------------------------------------------------------
module.exports = CleatEntryTransformer;
