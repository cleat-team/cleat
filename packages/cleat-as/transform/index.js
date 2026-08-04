"use strict";

const fs = require("fs");
const path = require("path");

// Properties that point back up the tree or into the compiler's own state.
// _walkCalls walks every other own property, so these have to be excluded by
// name: node.range.source.statements is the entire file, which would turn a
// walk of one function body into a walk of the whole program.
const WALK_SKIP_KEYS = new Set([
  "range", "source", "parent", "program", "parser", "tokenizer",
]);

/**
 * AssemblyScript transformer for the cleat durable execution framework.
 *
 * Generates WASM export wrappers for functions decorated with @cleatEntry.
 * The wrappers conform to the cleat ABI:
 *   (argsPtr: usize, argsLen: i32, outPtr: usize, maxOutLen: i32) => i64
 *
 * For a user function like:
 *
 *   @cleatEntry
 *   function placeOrder(h: HostCalls, input: PlaceOrderInput): string { ... }
 *
 * The transformer:
 *   1. Renames the original function to __durable_inner_placeOrder
 *   2. Strips the @cleatEntry decorator
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
    // Use this.program (set on the prototype by AS) instead of parser.program,
    // because AS 0.27.32+ does not set parser.program.  The parser argument
    // is still valid for parser.parseFile() calls in _injectWrappers.
    const program = this.program;
    if (!program || !program.sources) return;

    // Phase 1: Find @cleatEntry functions grouped by source file
    const sourceEntries = this._findDurableEntries(program);

    // Phase 1b: Static analysis - build call graphs, compute durable closure,
    //            validate functions, verify HostCalls threading
    for (const source of program.sources) {
      if (!source || !source.statements) continue;

      // Build call graph for all functions in this source
      const callGraph = this._buildCallGraph(source);

      // Find functions that directly call HostCalls methods
      const durableLeaves = this._findDurableLeaves(callGraph);

      // A @cleatEntry function IS durable: it is the workflow, and every line
      // of it replays. Seeding the closure only from _findDurableLeaves --
      // functions that make an h.* call -- meant an entry point that happens
      // not to call the host was never validated, which is precisely the
      // workflow whose only nondeterminism is a bare Math.random().
      for (const stmt of source.statements) {
        if (this._isDurableEntryFunc(stmt) && stmt.name && stmt.name.text) {
          durableLeaves.add(stmt.name.text);
        }
      }

      if (durableLeaves.size === 0) continue;

      // Compute transitive closure of durable functions
      const durableFunctions = this._computeDurableClosure(callGraph, durableLeaves);

      // Validate all functions in the durable closure for forbidden APIs
      for (const stmt of source.statements) {
        if (!stmt || !stmt.name || !stmt.signature) continue;
        if (durableFunctions.has(stmt.name.text)) {
          this._validateDurableFunction(stmt, source);
        }
      }

      // Verify that functions in the durable closure have access to 'h'
      this._verifyThreading(source, durableFunctions, callGraph);
    }

    // E001-E005 were `console.error` and nothing else, so `cleat build` on a
    // workflow calling Math.random() inside a durable function printed a
    // diagnostic, exited 0, and produced a deployable .wasm. Determinism
    // violations corrupt replay silently, which is the worst time to find out.
    // Throwing here is what makes asc fail.
    const violations = this._violations || [];
    if (violations.length > 0) {
      if (process.env.CLEAT_AS_ALLOW_NONDETERMINISM === "1") {
        console.error(
          "[cleat/transform] CLEAT_AS_ALLOW_NONDETERMINISM=1: continuing despite " +
          violations.length + " determinism violation(s). The resulting workflow " +
          "will not replay correctly. This escape hatch exists so a false positive " +
          "cannot block you -- please report one rather than leaving this set."
        );
      } else {
        throw new Error(
          "[cleat/transform] " + violations.length + " determinism violation(s); " +
          "see the E001-E005 diagnostics above. A workflow that is not " +
          "deterministic produces different results on replay than it did on the " +
          "original run. Set CLEAT_AS_ALLOW_NONDETERMINISM=1 to downgrade these to " +
          "warnings if you believe one is a false positive."
        );
      }
    }

    if (sourceEntries.length === 0) return;

    // Phase 2: Modify AST - rename functions and strip decorators
    this._renameEntries(sourceEntries);

    // Phase 3: Generate wrapper source code and inject into the source
    this._injectWrappers(parser, sourceEntries);
  }

  // ---------------------------------------------------------------
  // Phase 1: Walk all sources and collect @cleatEntry metadata
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
          const sourceName = source.internalPath || source.name || "unknown";
          const loc = this._getSourceLocation(stmt, sourceName);
          console.error(
            "[@cleat/transform] Warning: @cleatEntry function '" +
            (stmt.name ? stmt.name.text : "unknown") +
            "' at " + loc + " has no parameters." +
            " The first parameter must be HostCalls to access SDK methods." +
            " Without HostCalls, the workflow cannot make durable calls."
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
  // Check if a statement is a function declaration with @cleatEntry
  // ---------------------------------------------------------------
  _isDurableEntryFunc(stmt) {
    if (!stmt || typeof stmt !== "object") return false;
    // Must look like a function declaration with decorators
    if (!stmt.name || !stmt.signature || !stmt.decorators) return false;
    if (!Array.isArray(stmt.decorators) || stmt.decorators.length === 0) return false;
    // One of the decorators must be @cleatEntry
    return stmt.decorators.some(
      d => d && d.name && d.name.text === "cleatEntry"
    );
  }

  // ---------------------------------------------------------------
  // Extract metadata from a @cleatEntry function declaration
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
  // Phase 2: Rename @cleatEntry functions and strip decorators
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

        // Remove @cleatEntry decorator
        stmt.decorators = stmt.decorators.filter(
          d => !(d.name && d.name.text === "cleatEntry")
        );
      }
    }
  }

  // ---------------------------------------------------------------
  // Static analysis helpers
  // ---------------------------------------------------------------

  // ---------------------------------------------------------------
  // Validate a function body for forbidden API calls (Math.random,
  // Date.now, console.log, process.*) in the durable closure.
  // ---------------------------------------------------------------
  _validateDurableFunction(stmt, source) {
    const funcName = stmt.name ? stmt.name.text : "unknown";
    const sourceName = source.internalPath || source.name || "unknown";
    const body = stmt.body;
    if (!body || !body.statements) return;

    const self = this;
    self._walkCalls(body.statements, function(callExpr) {
      const calleeName = self._calleeName(callExpr);
      if (!calleeName) return;

      const dot = calleeName.indexOf(".");
      if (dot < 0) return; // plain function call, not a member access
      const objName = calleeName.substring(0, dot);
      const propName = calleeName.substring(dot + 1);

      const loc = self._getSourceLocation(callExpr, sourceName);

      if (objName === "Math" && (propName === "random" || propName === "seedRandom")) {
        self._reportViolation("E001: Math." + propName + "() in durable function '" + funcName + "' at " + loc + "\n  → Math.random() produces different values on each replay, breaking workflow determinism.\n  → Use h.random() for deterministic randomness.");
        return;
      }

      if (objName === "Date" && propName === "now") {
        self._reportViolation("E002: Date.now() in durable function '" + funcName + "' at " + loc + "\n  → Date.now() returns wall-clock time which differs across replays, breaking determinism.\n  → Use h.now() for deterministic time.");
        return;
      }

      if (objName === "console" && propName === "log") {
        self._reportViolation("E003: console.log() in durable function '" + funcName + "' at " + loc + "\n  → console.log() output is not recorded in workflow event history and is lost on replay.\n  → Use h.log() for durable logging.");
        return;
      }

      if (objName === "process") {
        self._reportViolation("E004: process." + propName + " in durable function '" + funcName + "' at " + loc + "\n  → Process/environment access differs across replays, breaking determinism.\n  → Pass configuration as workflow input instead of reading from process.env.");
        return;
      }
    });
  }

  // ---------------------------------------------------------------
  // Record a determinism violation. These are numbered E001..E005 --
  // they are errors, not warnings -- but for as long as they were only
  // console.error() calls, `cleat build` exited 0 and shipped the .wasm
  // anyway. Collect them here; afterParse() throws at the end if the list
  // is non-empty, which is what makes asc fail.
  // ---------------------------------------------------------------
  _reportViolation(message) {
    if (!this._violations) this._violations = [];
    this._violations.push(message);
    console.error("[cleat/transform] " + message);
  }

  // ---------------------------------------------------------------
  // Recursively walk AST statements and invoke callback for each
  // CallExpression node.
  //
  // This used to enumerate child properties by name -- statements,
  // expression, args, init, object, property, callee, condition,
  // consequent, alternate, declaration, value -- and test for a call with
  // `if (node.callee)`. Those are ESTree/Babel names. AssemblyScript's AST
  // uses different ones, and for the two that matter most it does not merely
  // differ, it has no such property at all:
  //
  //   looked for              AssemblyScript actually has
  //   node.callee             node.expression       (CallExpression)
  //   callee.object           expression.expression (PropertyAccessExpression)
  //   consequent / alternate  ifTrue / ifFalse      (IfStatement)
  //   declaration             declarations (array)  (VariableStatement)
  //   init                    initializer           (VariableDeclaration)
  //
  // So `node.callee` was never truthy on a real parse, the callback never
  // fired, every call graph came back empty, _findDurableLeaves returned an
  // empty set, and afterParse's `if (durableLeaves.size === 0) continue`
  // skipped validation for every file. The determinism checks E001-E004 and
  // the E005 threading check had never run on AssemblyScript source.
  //
  // Verified on examples/as-workflow before the fix: place_order calls
  // h.cleatCall seven times and its recorded callee set was [].
  //
  // Enumerating the correct names would fix today's grammar and silently rot
  // the next time AssemblyScript adds a node type. Walk every own property
  // instead: it cannot miss a node kind. The cost is having to skip the
  // properties that point back up -- node.range.source.statements is the
  // whole file, which would turn a function-body walk into a whole-program
  // walk -- and to guard against cycles.
  // ---------------------------------------------------------------
  _walkNodes(statements, visit) {
    if (!Array.isArray(statements)) return;

    const seen = new Set();

    function walkNode(node) {
      if (!node || typeof node !== 'object') return;
      if (seen.has(node)) return;
      seen.add(node);

      if (Array.isArray(node)) {
        for (const item of node) walkNode(item);
        return;
      }

      visit(node);

      for (const key of Object.keys(node)) {
        if (WALK_SKIP_KEYS.has(key)) continue;
        const child = node[key];
        if (child && typeof child === 'object') walkNode(child);
      }
    }

    for (const stmt of statements) {
      walkNode(stmt);
    }
  }

  // Visit every CallExpression under the given statements.
  _walkCalls(statements, callback) {
    const self = this;
    this._walkNodes(statements, function (node) {
      if (self._isCall(node)) callback(node);
    });
  }

  // ---------------------------------------------------------------
  // Does this function construct its own HostCalls?
  //
  // AssemblyScript's NewExpression carries `typeName` and `args` but no
  // `expression`, so it is deliberately not a call as far as _isCall is
  // concerned -- `new Foo()` is not a determinism violation and should not
  // enter the call graph as one.
  // ---------------------------------------------------------------
  _constructsHostCalls(stmt) {
    if (!stmt || !stmt.body || !stmt.body.statements) return false;
    let found = false;
    this._walkNodes(stmt.body.statements, function (node) {
      if (found || !node.typeName) return;
      const tn = node.typeName;
      const name = (tn.identifier && tn.identifier.text) || tn.text ||
        (tn.name && tn.name.text) || null;
      if (name === "HostCalls") found = true;
    });
    return found;
  }

  // ---------------------------------------------------------------
  // Structural test for a call node. Deliberately not `kind === NodeKind.Call`:
  // NodeKind is a numeric enum whose values shift when AssemblyScript inserts
  // a node type, and a stale number fails silently rather than loudly.
  // ---------------------------------------------------------------
  _isCall(node) {
    return Array.isArray(node.args) && !!node.expression;
  }

  // ---------------------------------------------------------------
  // Dotted name of the thing being called: "h.cleatCall", "Math.random",
  // or a bare "helperFunction". Returns null if the callee is something
  // more complicated than an identifier or a single property access.
  // ---------------------------------------------------------------
  _calleeName(callNode) {
    if (!callNode || !callNode.expression) return null;
    const target = callNode.expression;

    // PropertyAccessExpression: { expression: <object>, property: Identifier }
    if (target.property && target.expression) {
      const objName = target.expression.text ||
        (target.expression.name ? target.expression.name.text : null);
      const propName = target.property.text ||
        (target.property.name ? target.property.name.text : null);
      if (objName && propName) return objName + "." + propName;
      return null;
    }

    // IdentifierExpression: a direct call.
    if (target.text) return target.text;
    if (target.name && target.name.text) return target.name.text;
    return null;
  }

  // ---------------------------------------------------------------
  // Build a call graph for a source file: map caller -> set of callees
  // and callee -> set of callers (reverse edges).
  // ---------------------------------------------------------------
  _buildCallGraph(source) {
    const callers = {};
    const callees = {};

    for (const stmt of source.statements) {
      if (!stmt || !stmt.name || !stmt.signature) continue;
      const callerName = stmt.name.text;

      if (!stmt.body || !stmt.body.statements) continue;

      const calleeSet = new Set();
      const self = this;

      self._walkCalls(stmt.body.statements, function(callExpr) {
        const calleeName = self._calleeName(callExpr);
        if (calleeName) {
          calleeSet.add(calleeName);
        }
      });

      callers[callerName] = Array.from(calleeSet);
      for (const cn of calleeSet) {
        if (!callees[cn]) callees[cn] = [];
        callees[cn].push(callerName);
      }
    }

    return { callers, callees };
  }

  // ---------------------------------------------------------------
  // Find functions that directly call HostCalls methods (durable leaves).
  // ---------------------------------------------------------------
  _findDurableLeaves(callGraph) {
    const leaves = new Set();
    for (const [callerName, calleeNames] of Object.entries(callGraph.callers)) {
      for (const calleeName of calleeNames) {
        if (this._isHostCall(calleeName)) {
          leaves.add(callerName);
          break;
        }
      }
    }
    return leaves;
  }

  // ---------------------------------------------------------------
  // Compute the transitive closure of durable functions: starting from
  // durable leaves, traverse callers until fixed point.
  // ---------------------------------------------------------------
  _computeDurableClosure(callGraph, durableLeaves) {
    const durableFuncs = new Set(durableLeaves);

    let changed = true;
    while (changed) {
      changed = false;
      for (const [callerName, calleeNames] of Object.entries(callGraph.callers)) {
        if (durableFuncs.has(callerName)) continue;
        for (const calleeName of calleeNames) {
          // If the callee is already in the durable closure or is a HostCall
          if (durableFuncs.has(calleeName) || this._isHostCall(calleeName)) {
            durableFuncs.add(callerName);
            changed = true;
            break;
          }
        }
      }
    }

    return durableFuncs;
  }

  // ---------------------------------------------------------------
  // Verify that all functions in the durable closure have access to 'h'
  // (HostCalls parameter). Report errors with call chain trace.
  // ---------------------------------------------------------------
  _verifyThreading(source, durableFunctions, callGraph) {
    const sourceName = source.internalPath || source.name || "unknown";

    for (const stmt of source.statements) {
      if (!stmt || !stmt.name || !stmt.signature) continue;
      const funcName = stmt.name.text;
      if (!durableFunctions.has(funcName)) continue;

      // Check if the function has 'h' as first parameter
      const params = stmt.signature.parameters || [];
      const hasH = params.length > 0 && params[0] && params[0].name && params[0].name.text === "h";

      // ...or builds its own. A hand-written raw ABI export takes
      // (argsPtr, argsLen, outPtr, maxOutLen) and does `let h = new HostCalls()`
      // in its body -- it has host access, it just did not receive it from a
      // caller. examples/widget-store-as/assembly/workflows.ts is exactly this
      // shape, and the first time E005 ran for real it failed that build.
      // E005 exists to catch a durable helper that CANNOT reach the host, not
      // one that reaches it a different way.
      if (!hasH && !this._constructsHostCalls(stmt)) {
        const loc = this._getSourceLocation(stmt, sourceName);
        const chain = this._traceCallChain(funcName, callGraph);
        this._reportViolation(
          "E005: Durable function '" + funcName + "' at " + loc + " is missing HostCalls parameter 'h'.\n" +
          "  → Add 'h: HostCalls' as the first parameter.\n" +
          "  → Call chain: " + chain.join(" → ")
        );
      }
    }
  }

  // ---------------------------------------------------------------
  // Trace the call chain from a function back to an entry point.
  // Walks upward through callers in the reverse-edge graph.
  // ---------------------------------------------------------------
  _traceCallChain(funcName, callGraph) {
    const chain = [funcName];
    const visited = new Set();
    let current = funcName;

    while (current && !visited.has(current)) {
      visited.add(current);
      const callers = callGraph.callees[current] || [];
      if (callers.length === 0) break;
      current = callers[0];
      chain.unshift(current);
    }

    return chain;
  }

  // ---------------------------------------------------------------
  // Get formatted source location "(file:line)" from an AST node.
  // Tries node.range.start.line or node.name.range.start.line.
  // ---------------------------------------------------------------
  // AssemblyScript's Range is { start: number, end: number, source: Source }
  // -- start is a character offset, not a { line } object. The old
  // `range.start.line !== undefined` test was therefore never true, so every
  // diagnostic that did manage to fire pointed at a file with no line number.
  // Source.lineAt(pos) is how AssemblyScript itself resolves one.
  _getSourceLocation(node, sourceName) {
    const range = node.range || (node.name ? node.name.range : null);
    if (!range) return sourceName;

    if (range.source && typeof range.source.lineAt === "function" &&
        typeof range.start === "number") {
      const line = range.source.lineAt(range.start);
      const col = typeof range.source.columnAt === "function" ? range.source.columnAt() : 0;
      return sourceName + ":" + line + (col ? ":" + col : "");
    }

    // Pre-existing shape kept as a fallback for hand-built nodes in tests.
    if (range.start && range.start.line !== undefined) {
      return sourceName + ":" + range.start.line;
    }
    return sourceName;
  }

  // ---------------------------------------------------------------
  // Check if a callee name is a known HostCalls method (h.*).
  // ---------------------------------------------------------------
  _isHostCall(calleeName) {
    if (!calleeName || typeof calleeName !== 'string' || !calleeName.startsWith("h.")) return false;
    const method = calleeName.substring(2);
    const knownMethods = [
      "cleatCall", "cleatCallMs", "cleatSleep", "cleatSleepMs", "now",
      "random", "log", "version", "minVersion", "defer",
      "pollCancellation", "pollSignal", "continueAsNew",
      "childWorkflow", "childWorkflowWithOptions", "awaitChild",
      "awaitSignals", "awaitSignalsMs", "setQueryState",
      "createPromise", "awaitPromise", "awaitPromiseMs",
      "registerUpdateHandler", "pluginCall", "currentWorkflowId",
      "setScope", "getScope", "clearScope", "uuid",
      "sendSignalAndWait", "sendSignalAndWaitMs", "replyToSignal",
      "awaitSignalsWithQuorum", "awaitSignalsWithQuorumMs",
      "signalWorkflow", "resolvePromise", "rejectPromise", "cleatSend",
      "scheduleInvoke", "scheduleInvokeMs", "registerQueryHandler",
      "runDetached", "setState", "getState", "deleteState", "incrState",
      "hasState", "listState", "awaitAllChildren", "isReplaying",
      "currentRunId", "cleatFetch", "fetchGet", "acquireLock",
      "acquireLockMs", "releaseLock", "scheduleCron", "deleteCron",
      "listCrons",
    ];
    return knownMethods.includes(method);
  }

  // ---------------------------------------------------------------
  // Phase 3: Generate wrapper code and inject it into source files
  // ---------------------------------------------------------------
  _injectWrappers(parser, sourceEntries) {
    for (const { source, entries } of sourceEntries) {
      const needsImport = !this._hasCleatSdkImport(source);
      const needsJsonImport = this._hasMultiParamEntries(entries);
      const wrapperCode = this._generateWrappers(entries);

      try {
        // In AS 0.27.32+, parser.parseFile(sourceText, sourcePath, isLibrary)
        // parses code into the program but does NOT return the parsed source.
        // The parsed source is added to parser.sources array.
        // We capture the count before each call and find the new source after.

        const findNew = (nameHint) => {
          // Scan the entire sources array for a match
          for (const s of parser.sources) {
            if (!s) continue;
            const sourceName = s.internalPath || s.name || s.normalizedPath || "";
            if (sourceName.indexOf(nameHint) >= 0) return s;
          }
          return null;
        };

        // Insert @cleat/sdk import at the TOP of the source if needed
        if (needsImport) {
          let importLine = 'import { HostCalls, Memory, SUSPEND_SENTINEL, isWorkflowSuspended, resetWorkflowSuspended';
          if (needsJsonImport) {
            importLine += ', JsonParser, JsonVal';
          }
          importLine += ' } from "@cleat/sdk";\n';
          parser.parseFile(
            importLine,
            "generated/cleat-import.ts",
            false
          );
          const importSrc = findNew("cleat-import");
          if (importSrc && importSrc.statements) {
            for (let i = importSrc.statements.length - 1; i >= 0; i--) {
              const s = importSrc.statements[i];
              if (s) source.statements.unshift(s);
            }
          }
        }

        // Append wrapper functions at the END of the source
        parser.parseFile(
          wrapperCode,
          "generated/cleat-wrappers.ts",
          false
        );
        const wrapSrc = findNew("cleat-wrappers");
        if (wrapSrc && wrapSrc.statements) {
          for (const s of wrapSrc.statements) {
            if (s) source.statements.push(s);
          }
        } else {
          console.error("[@cleat/transform] Failed to inject wrapper AST; writing fallback file.");
          this._writeFallback(wrapperCode);
        }
      } catch (e) {
        console.error("[@cleat/transform] Failed to inject wrappers: " + e.message + ".");
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
    let outPath = "";
    try {
      const cwd = process.cwd();
      const outDir = path.join(cwd, "assembly", "generated");
      if (!fs.existsSync(outDir)) {
        fs.mkdirSync(outDir, { recursive: true });
      }
      outPath = path.join(outDir, "cleat-wrappers.ts");
      fs.writeFileSync(outPath, code, "utf-8");
      console.error(
        "[@cleat/transform] Wrote wrapper file to " + outPath +
        ". Add \"assembly/generated/cleat-wrappers.ts\" to the \"entries\" array in asconfig.json."
      );
    } catch (_e) {
      console.error(
        "[@cleat/transform] Could not write fallback wrapper file" +
        (outPath ? " to " + outPath : "") + "." +
        " Both AST injection and file output have failed." +
        " Check that the output directory is writable and there is disk space." +
        " Try running 'npx asc' directly without the transform."
      );
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
    code += "// The inner function receives the raw input JSON string and\n";
    code += "// returns a result JSON string. Suspension is detected via\n";
    code += "// the __workflowSuspended flag.\n\n";

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
    //              OR SUSPEND_SENTINEL if the workflow needs to suspend
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
    // Step 2: Create HostCalls instance (first param is always HostCalls)
    // ------------------------------------------------------------------
    code += `  // ---- Step 2: Create HostCalls instance ----\n`;
    code += `  const h = new HostCalls();\n\n`;

    // ------------------------------------------------------------------
    // Step 3: Reset suspension flag and invoke the workflow function
    // ------------------------------------------------------------------
    code += `  // ---- Step 3: Invoke the workflow function ----\n`;
    code += `  resetWorkflowSuspended();\n\n`;

    // Additional parameters beyond HostCalls are assigned from the raw JSON input
    if (paramNames.length === 0) {
      // No additional params -- just pass the HostCalls
      if (isVoid) {
        code += `  let _result: string = "";\n`;
        code += `  ${innerName}(h);\n`;
      } else {
        code += `  const _result: string = ${innerName}(h);\n`;
      }
    } else if (paramNames.length === 1) {
      // Single additional param -- pass the raw JSON string
      code += `  const ${paramNames[0]}: string = argsJson;\n`;
      if (isVoid) {
        code += `  let _result: string = "";\n`;
        code += `  ${innerName}(h, ${paramNames[0]});\n`;
      } else {
        code += `  const _result: string = ${innerName}(h, ${paramNames[0]});\n`;
      }
    } else {
      // Multiple additional params -- parse JSON and extract each field
      code += `  // ---- Parse argsJson for multi-param entry ----\n`;
      code += `  let _parser = new JsonParser();\n`;
      code += `  let _parsed: JsonVal | null = _parser.parse(argsJson);\n`;
      code += `  if (_parsed === null) {\n`;
      code += `    const _errBody = '{"error":"invalid input JSON for multi-param entry"}';\n`;
      code += `    const _errWritten = Memory.writeString(outPtr, maxOutLen, _errBody);\n`;
      code += `    return Memory.encodeExportResult(1, _errWritten);\n`;
      code += `  }\n\n`;
      code += `  // Extract named params\n`;
      for (let i = 0; i < paramNames.length; i++) {
        code += this._getDeserializeCode(paramNames[i], paramTypes[i], funcName);
      }
      if (isVoid) {
        code += `  let _result: string = "";\n`;
        code += `  ${innerName}(h, ${paramNames.join(", ")});\n`;
      } else {
        code += `  const _result: string = ${innerName}(h, ${paramNames.join(", ")});\n`;
      }
    }
    code += `\n`;

    // ------------------------------------------------------------------
    // Step 4: Check for suspension
    // ------------------------------------------------------------------
    code += `  // ---- Step 4: Check for suspension ----\n`;
    code += `  if (isWorkflowSuspended()) {\n`;
    code += `    return SUSPEND_SENTINEL;\n`;
    code += `  }\n\n`;

    // ------------------------------------------------------------------
    // Step 5: Write result to output buffer and return
    // ------------------------------------------------------------------
    if (isVoid) {
      code += `  // ---- Step 5: Write success response ----\n`;
      code += `  const _out = '{"ok":true}';\n`;
      code += `  const _written = Memory.writeString(outPtr, maxOutLen, _out);\n`;
      code += `  return Memory.encodeExportResult(0, _written);\n`;
    } else {
      // String or object return: write the result directly to output
      code += `  // ---- Step 5: Write result to output buffer ----\n`;
      code += `  const _written = Memory.writeString(outPtr, maxOutLen, _result);\n`;
      code += `  return Memory.encodeExportResult(0, _written);\n`;
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
  // Check if any entry in the list has multiple user params
  // ---------------------------------------------------------------
  _hasMultiParamEntries(entries) {
    return entries.some(e => e.paramNames.length > 1);
  }

  // ---------------------------------------------------------------
  // Generate deserialization code for a single parameter based on type
  // Used by the multi-parameter branch to select the correct JSON getter
  // ---------------------------------------------------------------
  _getDeserializeCode(pname, ptype, funcName) {
    // Map AS types to the correct JsonParser getter
    if (ptype === "string" || ptype === "String") {
      return `  let ${pname}: string = _parser.getString(_parsed, "${pname}");\n`;
    } else if (ptype === "i32") {
      return `  let ${pname}: i32 = <i32>_parser.getNumber(_parsed, "${pname}");\n`;
    } else if (ptype === "u32") {
      return `  let ${pname}: u32 = <u32>_parser.getNumber(_parsed, "${pname}");\n`;
    } else if (ptype === "i64") {
      return `  let ${pname}: i64 = <i64>_parser.getNumber(_parsed, "${pname}");\n`;
    } else if (ptype === "u64") {
      return `  let ${pname}: u64 = <u64>_parser.getNumber(_parsed, "${pname}");\n`;
    } else if (ptype === "f64") {
      return `  let ${pname}: f64 = _parser.getNumber(_parsed, "${pname}");\n`;
    } else if (ptype === "f32") {
      return `  let ${pname}: f32 = <f32>_parser.getNumber(_parsed, "${pname}");\n`;
    } else if (ptype === "bool" || ptype === "boolean") {
      return `  let ${pname}: bool = _parser.getBool(_parsed, "${pname}");\n`;
    } else {
      // Unknown type — throw a compile-time error from the transformer
      throw new Error(
        "[@cleat/transform] Unsupported type '" + ptype +
        "' for parameter '" + pname + "' in function '" + funcName +
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
