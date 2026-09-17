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

      // Scope for both checks: what the workflow can REACH. cleat#1799.
      //
      // durableLeaves is already {functions calling h.*} + {@cleatEntry
      // functions}, which are exactly the right roots for a forward walk.
      //
      // The backward walk is kept for a source that declares no entry point,
      // where the forward one would have only leaf callers to start from and
      // would stop checking whatever calls them.
      const hasEntry = source.statements.some(st => this._isDurableEntryFunc(st));
      const durableFunctions = hasEntry
        ? this._computeReachable(callGraph, durableLeaves)
        : this._computeDurableClosure(callGraph, durableLeaves);

      // The THREADING scope is the INTERSECTION, and neither closure alone
      // works -- both obvious answers are wrong, in opposite directions.
      //
      //   the callers closure alone flags the boundary that SUPPLIES h: a
      //     harness that constructs a HostCalls and calls the workflow;
      //   durableFunctions alone flags helpers that NEED no h. Measured here:
      //     substituting it made SIX pure helpers in examples/as-workflow --
      //     extractStringField, extractI64Field, extractRawArray, indexOf,
      //     isDigit, parseI64 -- fail E005 on an UNMODIFIED example, taking
      //     the control build from green to seven diagnostics.
      //
      // E005 asks "can this function obtain the h it needs?", which is only a
      // meaningful question for a function that BOTH participates in the
      // workflow and reaches a host call.
      //
      //   isDigit   forward yes, backward no   -> excluded, needs no h
      //   a harness forward no,  backward yes  -> excluded, supplies h
      //   a durable helper missing h           -> in both, reported
      //
      // The Python checker had the identical pair; see cleat#1813/#1816, where
      // the intersection is pinned from both sides by two tests.
      const reachesHost = this._computeDurableClosure(callGraph, durableLeaves);
      const threadingScope = hasEntry
        ? new Set([...durableFunctions].filter(f => reachesHost.has(f)))
        : reachesHost;

      // Validate all functions in the durable closure for forbidden APIs
      for (const stmt of source.statements) {
        if (!stmt || !stmt.name || !stmt.signature) continue;
        if (durableFunctions.has(stmt.name.text)) {
          this._validateDurableFunction(stmt, source);
        }
      }

      // Verify that functions in the durable closure have access to 'h'
      this._verifyThreading(source, threadingScope, callGraph);
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

    const paramDefaults = [];

    for (const p of userParams) {
      const pName = p.name && p.name.text ? p.name.text : "_";
      // THE DECLARED DEFAULT, AS SOURCE TEXT. cleat#1065.
      //
      // AssemblyScript supports `note: string = "FALLBACK"`, and this transform
      // discarded it: the generated wrapper calls the inner function with EVERY
      // argument explicitly, so AS's own default never fires, and an absent key
      // bound the getter's zero ("" for a string) instead. Measured by driving
      // _generateWrappers with a defaulted parameter -- the emitted wrapper did
      // not mention the default at all.
      //
      // Source text rather than the parsed value, because a default is an
      // arbitrary expression and only its text reproduces it faithfully. The
      // shape was confirmed against a REAL asc run, not a mock: the initializer
      // carries {kind, range, literalKind, value} and range.source.text yields
      // exactly `"FALLBACK"` and `7` for the two cases below. A mock could not
      // have answered this -- cleat#1067 is in this file because a mock was
      // built to satisfy the implementation rather than to model the parser.
      let pDefault = null;
      const init = p.initializer;
      if (init && init.range && init.range.source &&
          typeof init.range.source.text === "string") {
        pDefault = init.range.source.text.slice(init.range.start, init.range.end);
      }
      paramDefaults.push(pDefault);
      // The declared type. `name.identifier.text` first, `.text` second --
      // the same order this file already uses for type names at the
      // _typeName helper below.
      //
      // This site read `.text` ALONE: `p.type && p.type.text ? ... : "string"`.
      // An AssemblyScript type node has no `text` property -- its keys are
      // kind/range/isNullable/currentlyResolving/name/typeArguments -- so the
      // condition was always false and every parameter silently became
      // "string". Measured 2026-09-09 by dumping the node during a real build:
      // p.type.text was undefined and p.type.name.identifier.text was
      // "string", "string", "i32".
      //
      // Effect: a multi-parameter entry point failed to compile for any
      // non-string parameter, because the wrapper bound it with getString --
      //   ERROR TS2322: Type '~lib/string/String' is not assignable to 'i32'
      // -- and _getDeserializeCode's i32/u32/i64/u64/f64/f32/bool branches
      // were unreachable, including its own "Unsupported type" throw. The
      // fallback answered first, every time.
      //
      // Throw rather than default to a type. No default can be correct for a
      // parameter whose type could not be read, and picking one is exactly
      // what hid this: a wrong answer that compiles for the common case.
      const pTypeNode = p.type;
      const pType =
        (pTypeNode && pTypeNode.name && pTypeNode.name.identifier &&
          pTypeNode.name.identifier.text) ||
        (pTypeNode && pTypeNode.text);
      if (!pType) {
        throw new Error(
          "[@cleat/transform] could not read the declared type of parameter '" +
          pName + "' in '" + funcName + "'. This is a transform bug, not a " +
          "problem with your workflow -- please report it."
        );
      }

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
      paramDefaults,
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
  // Functions reachable FORWARD from the durable roots: everything an
  // entry point, or a function that calls the host, can itself call.
  //
  // This is the scope both checks need. Determinism is a property of what a
  // workflow EXECUTES, so the question is "what does the entry reach?", not
  // "who reaches into this?".
  //
  // Answering it backwards was cleat#1799, and it was wrong in BOTH directions
  // at once, measured 2026-09-17 on a copy of examples/as-workflow:
  //
  //   a helper the workflow CALLS, doing Date.now()   0 diagnostics, .wasm written
  //   a CALLER of the workflow, doing Date.now()      E002, build refused
  //
  // The first is the miss this issue was filed for. The second was not in the
  // report and is the more damaging half: it is a test harness or a __main__
  // driver being told its code is non-deterministic. Python's checker had the
  // identical pair (cleat#1813 / #1816), where a mock-driven harness produced
  // 22 false PY010s on a shipped example and refused its documented build.
  //
  // Note callGraph.callers maps a function to its CALLEES despite the name;
  // walking it forward is what makes this the callee closure.
  // ---------------------------------------------------------------
  _computeReachable(callGraph, roots) {
    const reachable = new Set();
    const queue = Array.from(roots);

    while (queue.length > 0) {
      const func = queue.shift();
      if (reachable.has(func)) continue;
      reachable.add(func);
      for (const callee of (callGraph.callers[func] || [])) {
        if (!reachable.has(callee)) queue.push(callee);
      }
    }

    return reachable;
  }

  // ---------------------------------------------------------------
  // Compute the transitive closure of durable functions: starting from
  // durable leaves, traverse callers until fixed point.
  //
  // RETAINED ONLY as the no-entry fallback -- see afterParse. A source with no
  // @cleatEntry gives the forward walk nothing but its leaf callers to start
  // from, which would stop checking any function that calls one. Keeping the
  // backward walk there leaves such files behaving exactly as they did, rather
  // than trading cleat#1799's false positives for a silent new false negative.
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
      "scheduleInvoke", "scheduleInvokeMs",
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

        // Insert the @cleat/sdk import at the TOP of the source. Always --
        // including when the author already imports from "@cleat/sdk".
        //
        // This was guarded by `if (!this._hasCleatSdkImport(source))` until
        // 2026-09-02, and the guard never fired. Measured with a probe on the
        // real AS parser: for a source whose first line is
        // `import { HostCalls, cleatEntry } from "@cleat/sdk"`, the detector
        // returned FALSE. It probes four property paths for an import's module
        // name and the AS AST matches none of them.
        //
        // So every generated wrapper has resolved Memory, SUSPEND_SENTINEL,
        // isWorkflowSuspended, resetWorkflowSuspended and runDeferred *because
        // the detector was broken*. A future asc that exposes one of those
        // property paths would have started suppressing this import and broken
        // every wrapper whose author had not happened to import all five by
        // hand -- a latent break with no test in front of it.
        //
        // Detection is not worth fixing. A working detector would have to
        // inject only the names the author left out, name by name, which is
        // strictly more machinery for no benefit. Injecting unconditionally is
        // what already happens, and duplicate-importing the same symbol from
        // the same module is fine: TestASTransform/compiles_when_the_user_
        // imports_the_same_symbols compiles a workflow that imports all five
        // itself.
        {
          let importLine = 'import { HostCalls, Memory, SUSPEND_SENTINEL, isWorkflowSuspended, resetWorkflowSuspended, runDeferred';
          if (needsJsonImport) {
            // TYPE_* accompany JsonParser: the generated multi-parameter
            // binder compares typeOf() against them by NAME rather than by
            // literal, so they must be in scope wherever JsonParser is.
            importLine += ', JsonParser, JsonVal, TYPE_STRING, TYPE_NUMBER, TYPE_BOOL';
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

    // Only for a module that actually has a workflow in it. A source with no
    // @cleatEntry must produce NO exports at all -- asserted by
    // TestASTransform/no_entry_no_wrapper, which this failed when the export
    // was emitted unconditionally.
    if (entries.length > 0) {
      code += this._generateDeferRunnerExport();
    }

    return code;
  }

  // ---------------------------------------------------------------
  // Generate the host's kill-path defer entry point
  //
  // IMPROVEMENT-PLAN 3.35 phase 4 / 3.73 piece 4. The wrapper above drains
  // the defer table when a workflow returns, which covers every workflow that
  // gets to return. A workflow the HOST killed -- execution fence, instruction
  // limit, memory ceiling -- never reaches it, and its cleanup would simply
  // never happen. `runGuestDefersAfterKill` in engine/backend_wasmtime.go
  // looks up this export by name and calls it.
  //
  // Emitted once per source that has at least one entry point, because it is
  // one export for the whole module rather than one per workflow. Emitting it
  // per entry would be a duplicate-export compile error the moment a module
  // declares two workflows.
  //
  // This has no error handling, and cannot have any: the SDK builds with
  // `--runtime stub`, which has no exceptions, so there is nothing to catch. A
  // defer body that aborts traps the instance -- which is a guest already
  // being torn down, so the trap costs nothing beyond the remaining bodies.
  // The Go equivalent recovers; this one has no equivalent to recover with.
  // ---------------------------------------------------------------
  _generateDeferRunnerExport() {
    let code = "";
    code += `// ---- Host-called defer entry point ----\n`;
    code += `// Called by the host for a workflow it killed, and for a defer\n`;
    code += `// segment, neither of which reaches the drain in the wrappers above.\n`;
    code += `// Returns how many bodies ran.\n`;
    code += `// Idempotent: the table is drained before the first body runs, so a\n`;
    code += `// guest that already ran its defers returns 0 here.\n`;
    code += `//\n`;
    code += `// resetWorkflowSuspended() first, and it is load-bearing rather than\n`;
    code += `// tidy. This is a HOST ENTRY POINT, and the flag means "the thing\n`;
    code += `// currently running asked to suspend" -- so every entry from the host\n`;
    code += `// must start with it clear, which is why Step 3 of each workflow\n`;
    code += `// wrapper does the same. Without it the flag arrives already set from\n`;
    code += `// the segment that just ended (a defer segment stops the body's call\n`;
    code += `// by setting exactly this flag), runDeferred reads it after the FIRST\n`;
    code += `// body as "that body suspended", and stops -- running one defer and\n`;
    code += `// silently dropping the rest. Measured 2026-09-03 on a two-defer\n`;
    code += `// fixture: defers_run=1, operations [second], the first cleanup gone.\n`;
    code += `// See engine/as_defer_segment_e2e_test.go.\n`;
    code += `export function __cleat_run_deferred(): i64 {\n`;
    code += `  resetWorkflowSuspended();\n`;
    code += `  return <i64>runDeferred(new HostCalls());\n`;
    code += `}\n\n`;
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
      paramDefaults,
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
    } else if (
      paramNames.length === 1 &&
      (paramTypes[0] === "string" || paramTypes[0] === "String") &&
      !(paramDefaults && paramDefaults[0])
    ) {
      // Single additional param -- pass the raw JSON string.
      //
      // GATED ON THE TYPE AND THE DEFAULT, not on the count alone (cleat#1065).
      // This branch assigns argsJson to the parameter, so it is only correct
      // when that parameter is a string that wants the whole payload. It used
      // to fire on `paramNames.length === 1` by itself, which produced two
      // separate defects:
      //
      //   1. A lone NON-STRING parameter did not compile AT ALL. The wrapper
      //      emitted `const count: string = argsJson;` and passed it to a
      //      function declaring i32, so asc reported
      //      "Type '~lib/string/String' is not assignable to type 'i32'"
      //      against generated/cleat-wrappers.ts -- a line the author never
      //      wrote. Go's equivalent fast path has always been gated on the
      //      type; AssemblyScript's was gated on the count.
      //
      //   2. A lone parameter with a declaration-site DEFAULT silently ignored
      //      it. Defaults are honoured by _getDeserializeCode, which only the
      //      named-extraction branch below calls, so `note: string = "X"`
      //      bound the raw payload instead of "X" -- defeating the optional
      //      mechanism for exactly the one-parameter case.
      //
      // Both are strictly additive to fix: case 1 could not compile, so no
      // workflow can depend on it, and case 2 could not have been relied on
      // either, since the declared default was never reachable. The
      // documented lone-STRING behaviour is unchanged, and
      // tests/conformance/entry_point_binding_cases.json's "lone string
      // parameter" row asserts it stays that way.
      code += `  const ${paramNames[0]}: string = argsJson;\n`;
      if (isVoid) {
        code += `  let _result: string = "";\n`;
        code += `  ${innerName}(h, ${paramNames[0]});\n`;
      } else {
        code += `  const _result: string = ${innerName}(h, ${paramNames[0]});\n`;
      }
    } else {
      // Named extraction: every param bound by name from the payload.
      //
      // Reached by multi-parameter entries, and -- since cleat#1065 -- also by
      // a SINGLE parameter that is not a bare string or that declares a
      // default. Both need the parse; only a lone defaultless string can skip
      // it.
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
        code += this._getDeserializeCode(paramNames[i], paramTypes[i], funcName,
          paramDefaults ? paramDefaults[i] : null);
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
    // Step 4b: Run guest-side defer bodies (IMPROVEMENT-PLAN 3.73)
    //
    // AFTER the suspension check, and that order is the whole point. A
    // suspended workflow has not exited; its defers are still pending, and
    // firing them at the first cleatSleep would release locks a workflow that
    // is about to continue still holds.
    //
    // The other SDKs get this ordering for free, because they suspend by
    // unwinding and the drain sits on a path the unwind skips. AssemblyScript
    // has no exceptions, so the entry point returns normally either way and
    // the guard has to be written out. An unguarded drain here compiles, runs,
    // and is wrong.
    //
    // The return value is deliberately discarded: the workflow's result is
    // already decided by the time this runs, and a defer cannot change it.
    // ------------------------------------------------------------------
    code += `  // ---- Step 4b: Run deferred cleanup (not on the suspend path) ----\n`;
    code += `  runDeferred(h);\n\n`;

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
  // The JsonVal type a parameter of `ptype` must have when present. Throws for
  // anything _getDeserializeCode cannot bind, so the two lists cannot drift.
  _expectedJsonType(ptype, pname, funcName) {
    if (ptype === "string" || ptype === "String") return "TYPE_STRING";
    if (ptype === "bool" || ptype === "boolean") return "TYPE_BOOL";
    if (["i32", "u32", "i64", "u64", "f64", "f32"].indexOf(ptype) !== -1) return "TYPE_NUMBER";
    throw new Error(
      "[@cleat/transform] Unsupported type '" + ptype +
      "' for parameter '" + pname + "' in function '" + funcName +
      "'. Supported types: string, i32, u32, i64, u64, f64, f32, bool"
    );
  }

  // Refuse a WRONG-TYPED argument; an ABSENT one still binds zero. cleat#1067.
  //
  // The getters collapse both into a zero value: getString returns "" for a
  // missing key AND for {"note": 42}, so a caller who sent the wrong type was
  // indistinguishable from one who sent nothing, and the workflow ran on the
  // zero value either way. Measured 2026-09-09 -- present "hi" gave length 2,
  // absent gave 0, and {"note": 42} also gave 0.
  //
  // Only the wrong-typed half changes. Absent must keep binding zero: it is how
  // an AS workflow expresses an optional parameter, since AS has no
  // Python-style defaults and this transform rejects composite types at compile
  // time. That matches the Go contract pinned by cleat#1061, and Go, Rust and
  // Java already refuse a wrong-typed argument -- AS was alone in accepting one.
  //
  // typeOf() returns -1 for an absent key, so `!= -1` is the present test and
  // the next comparison is the type check. Named constants rather than the
  // literals 1/2/3, so renumbering them in json.ts cannot silently invert it.
  _typeGuard(pname, ptype, funcName) {
    const expect = this._expectedJsonType(ptype, pname, funcName);
    return (
      `  const _t_${pname}: i32 = _parser.typeOf(_parsed, "${pname}");\n` +
      `  if (_t_${pname} != -1 && _t_${pname} != ${expect}) {\n` +
      this._makeErrorReturn(`parameter ${pname} has the wrong JSON type`) +
      `  }\n`
    );
  }

  // A DECLARED DEFAULT MAKES A PARAMETER OPTIONAL. cleat#1065.
  //
  // `pdefault` is the default's source text, or null. When present, an ABSENT
  // key binds it instead of the getter's zero value.
  //
  // WHY THIS IS THE MECHANISM AND NOT A NEW SYNTAX. The contract decided on
  // cleat#1065 is "an absent declared parameter is an error, unless the
  // parameter is declared optional". Python spells that with a parameter
  // default and Rust with Option<T>/#[serde(default)]; AssemblyScript has
  // neither nullable primitives nor a tag convention, but it DOES have default
  // parameter values -- the same spelling as Python. So the mechanism already
  // existed in the language and was being discarded here.
  //
  // `_t_<name> == -1` is the absence test the type guard above already emits;
  // typeOf returns -1 for a missing key. Reusing it means absence has ONE
  // definition in this wrapper rather than two that can disagree.
  //
  // ADDITIVE. Without a default the emitted code is unchanged, so an absent
  // parameter still binds zero until the contract flips.
  _getDeserializeCode(pname, ptype, funcName, pdefault) {
    // Map AS types to the correct JsonParser getter
    const g = this._typeGuard(pname, ptype, funcName);
    const hasDefault = !(pdefault === null || pdefault === undefined);
    const orDefault = (expr) =>
      !hasDefault ? expr : `_t_${pname} == -1 ? (${pdefault}) : (${expr})`;

    // AN ABSENT DECLARED PARAMETER IS AN ERROR. cleat#1065 step 4.
    //
    // Without a default, absence used to bind the getter's zero value -- ""
    // for a string, 0 for a number, false for a bool -- which CANNOT TELL
    // "sent zero" from "sent nothing", permanently, for every caller. The
    // workflow runs, the result is plausible, and nothing records that the
    // value is not the one that was sent.
    //
    // The information is the CALLER'S, and a declared default is the workflow
    // author saying absence is meaningful. So absence is refused unless that
    // declaration is present, which is the same rule Python has always had and
    // the one Rust gets from Option<T>.
    //
    // It reuses `_t_<name> == -1` rather than introducing a second absence
    // test, for the reason the comment above gives: one definition of absence
    // in this wrapper cannot disagree with itself.
    //
    // NOT the lone-string fast path, which never reaches this function: a
    // single defaultless string parameter receives the whole payload rather
    // than a value looked up by name, so "absent" does not apply to it.
    const requirePresent = hasDefault
      ? ""
      : `  if (_t_${pname} == -1) {\n` +
        this._makeErrorReturn(
          `entry point parameter ${pname} is absent from the start payload. ` +
          `An absent declared parameter is an error (cleat#1065): it cannot be told ` +
          `apart from one sent as the zero value, and that distinction belongs to the ` +
          `caller. Send ${pname}, or give it a default to declare it optional.`
        ) +
        `  }\n`;
    const g2 = g + requirePresent;
    if (ptype === "string" || ptype === "String") {
      return g2 + `  let ${pname}: string = ${orDefault(`_parser.getString(_parsed, "${pname}")`)};\n`;
    } else if (ptype === "i32") {
      return g2 + `  let ${pname}: i32 = ${orDefault(`<i32>_parser.getNumber(_parsed, "${pname}")`)};\n`;
    } else if (ptype === "u32") {
      return g2 + `  let ${pname}: u32 = ${orDefault(`<u32>_parser.getNumber(_parsed, "${pname}")`)};\n`;
    } else if (ptype === "i64") {
      return g2 + `  let ${pname}: i64 = ${orDefault(`<i64>_parser.getNumber(_parsed, "${pname}")`)};\n`;
    } else if (ptype === "u64") {
      return g2 + `  let ${pname}: u64 = ${orDefault(`<u64>_parser.getNumber(_parsed, "${pname}")`)};\n`;
    } else if (ptype === "f64") {
      return g2 + `  let ${pname}: f64 = ${orDefault(`_parser.getNumber(_parsed, "${pname}")`)};\n`;
    } else if (ptype === "f32") {
      return g2 + `  let ${pname}: f32 = ${orDefault(`<f32>_parser.getNumber(_parsed, "${pname}")`)};\n`;
    } else if (ptype === "bool" || ptype === "boolean") {
      return g2 + `  let ${pname}: bool = ${orDefault(`_parser.getBool(_parsed, "${pname}")`)};\n`;
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
