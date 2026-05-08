#!/usr/bin/env node
/**
 * inject-metadata.js — Post-compile metadata stamping for AS WASM workflows.
 *
 * Reads a compiled WASM binary and injects (or replaces) the "cleat.metadata"
 * custom section (section ID 0x00) with workflow identity information.
 *
 * Usage:
 *   node scripts/inject-metadata.js <wasm-file> \
 *     --name PlaceOrder \
 *     --version 3 \
 *     --min-version 1 \
 *     --abi-version 1 \
 *     --plugin-deps '{"llm":">=1.2.0"}' \
 *     [--output <output.wasm>]
 *
 * Environment variable alternative:
 *   CLEAT_WORKFLOW_NAME=PlaceOrder \
 *   CLEAT_WORKFLOW_VERSION=3 \
 *   CLEAT_MIN_COMPATIBLE_VERSION=1 \
 *   CLEAT_ABI_VERSION=1 \
 *   CLEAT_PLUGIN_DEPS='{"llm":">=1.2.0"}' \
 *   node scripts/inject-metadata.js <wasm-file>
 *
 * This is a post-compile step — run AFTER "asc" has produced the .wasm file.
 * It requires no external WASM libraries — pure Node.js Buffer manipulation.
 */

const fs = require("fs");
const path = require("path");

const SECTION_NAME = "cleat.metadata";
const WASM_MAGIC = Buffer.from([0x00, 0x61, 0x73, 0x6D]); // \0asm
const WASM_VERSION = Buffer.from([0x01, 0x00, 0x00, 0x00]); // v1

/**
 * Encode a number as unsigned LEB128 varint.
 */
function encodeULEB128(value) {
  const bytes = [];
  do {
    let b = value & 0x7f;
    value >>>= 7;
    if (value !== 0) b |= 0x80;
    bytes.push(b);
  } while (value !== 0);
  return Buffer.from(bytes);
}

/**
 * Decode an unsigned LEB128 varint at the given offset.
 * Returns { value, bytesRead }.
 */
function decodeULEB128(buf, offset) {
  let result = 0;
  let shift = 0;
  let bytesRead = 0;

  for (let i = offset; i < buf.length; i++) {
    bytesRead++;
    const b = buf[i];
    result |= (b & 0x7f) << shift;
    if ((b & 0x80) === 0) {
      return { value: result, bytesRead };
    }
    shift += 7;
    if (shift > 63) {
      throw new Error("invalid WASM binary: corrupted section length encoding");
    }
  }
  throw new Error("invalid WASM binary: truncated section length encoding");
}

/**
 * Build a complete custom section.
 * Section format: [section_id: 0x00] [payload_len: LEB128] [name_len: LEB128] [name] [payload]
 */
function buildCustomSection(name, payload) {
  const nameBytes = Buffer.from(name, "utf8");
  const nameLen = encodeULEB128(nameBytes.length);
  const fullPayload = Buffer.concat([nameLen, nameBytes, payload]);
  const payloadLen = encodeULEB128(fullPayload.length);
  return Buffer.concat([Buffer.from([0x00]), payloadLen, fullPayload]);
}

/**
 * Find a custom section in a WASM binary.
 * Returns { payload, sectionStart, sectionEnd } or null if not found.
 */
function findCustomSection(wasmBuf, name, filePath = "") {
  if (wasmBuf.length < 8) return null;

  // Validate magic.
  if (wasmBuf.slice(0, 4).compare(WASM_MAGIC) !== 0) {
    throw new Error("invalid WASM binary: bad magic number in '" + filePath + "'");
  }
  if (wasmBuf.slice(4, 8).compare(WASM_VERSION) !== 0) {
    throw new Error("invalid WASM binary: unsupported version '" + wasmBuf.slice(4, 8).toString("hex") + "' (only v1 supported) in '" + filePath + "'");
  }

  const nameBytes = Buffer.from(name, "utf8");
  let offset = 8;

  while (offset < wasmBuf.length) {
    const sectionStart = offset;
    const sectionId = wasmBuf[offset];
    offset++;

    if (offset >= wasmBuf.length) break;

    const { value: payloadLen, bytesRead } = decodeULEB128(wasmBuf, offset);
    offset += bytesRead;
    const sectionEnd = offset + payloadLen;

    if (offset + payloadLen > wasmBuf.length) {
      throw new Error("Section extends beyond end of WASM binary");
    }

    if (sectionId === 0) {
      // Custom section — check the name.
      const { value: nameLen, bytesRead: nameBytesRead } = decodeULEB128(wasmBuf, offset);
      if (nameBytesRead > 0) {
        const nameStart = offset + nameBytesRead;
        const nameEnd = nameStart + nameLen;
        if (nameEnd <= offset + payloadLen) {
          const sectionName = wasmBuf.slice(nameStart, nameEnd);
          if (sectionName.compare(nameBytes) === 0) {
            return {
              payload: wasmBuf.slice(nameEnd, sectionEnd),
              sectionStart,
              sectionEnd,
            };
          }
        }
      }
    }

    offset += payloadLen;
  }

  return null;
}

/**
 * Inject (or replace) the cleat.metadata custom section in a WASM binary.
 */
function injectMetadata(wasmBytes, meta, filePath = "") {
  const jsonPayload = Buffer.from(JSON.stringify(meta), "utf8");

  // Remove existing cleat.metadata section if present.
  const existing = findCustomSection(wasmBytes, SECTION_NAME, filePath);
  let base = wasmBytes;
  if (existing) {
    base = Buffer.concat([
      wasmBytes.slice(0, existing.sectionStart),
      wasmBytes.slice(existing.sectionEnd),
    ]);
  }

  const newSection = buildCustomSection(SECTION_NAME, jsonPayload);
  return Buffer.concat([base, newSection]);
}

/**
 * Read cleat.metadata from a WASM binary.
 */
function readMetadata(wasmBytes, filePath = "") {
  const found = findCustomSection(wasmBytes, SECTION_NAME, filePath);
  if (!found) return null;
  return JSON.parse(found.payload.toString("utf8"));
}

function main() {
  const args = process.argv.slice(2);

  if (args.length === 0 || args.includes("--help") || args.includes("-h")) {
    console.log(`
Usage: node scripts/inject-metadata.js <wasm-file> [options]

Options:
  --name <name>         Workflow name (or CLEAT_WORKFLOW_NAME env var)
  --version <n>         Workflow version (or CLEAT_WORKFLOW_VERSION env var)
  --min-version <n>     Min compatible version (or CLEAT_MIN_COMPATIBLE_VERSION env var)
  --abi-version <n>     ABI version (or CLEAT_ABI_VERSION env var)
  --plugin-deps <json>  Plugin dependencies JSON (or CLEAT_PLUGIN_DEPS env var)
  --output, -o <file>   Output WASM path (default: overwrite input)
  --read                Read and display metadata instead of writing
  --verbose, -v         Verbose output
`);
    process.exit(0);
  }

  const readOnly = args.includes("--read");
  let wasmFile = null;
  const options = {};

  for (let i = 0; i < args.length; i++) {
    const arg = args[i];
    if (arg === "--read" || arg === "--verbose" || arg === "-v") continue;
    if (arg === "--output" || arg === "-o") {
      options.output = args[++i];
    } else if (arg === "--name") {
      options.name = args[++i];
    } else if (arg === "--version") {
      options.version = parseInt(args[++i], 10);
    } else if (arg === "--min-version") {
      options.minVersion = parseInt(args[++i], 10);
    } else if (arg === "--abi-version") {
      options.abiVersion = parseInt(args[++i], 10);
    } else if (arg === "--plugin-deps") {
      options.pluginDeps = args[++i];
    } else if (!arg.startsWith("--")) {
      wasmFile = arg;
    }
  }

  if (!wasmFile) {
    console.error("Error: <wasm-file> is required");
    process.exit(1);
  }

  const wasmBytes = fs.readFileSync(wasmFile);

  if (readOnly) {
    const meta = readMetadata(wasmBytes, wasmFile);
    if (!meta) {
      console.error(`No '${SECTION_NAME}' section found in ${wasmFile}`);
      process.exit(1);
    }
    console.log(JSON.stringify(meta, null, 2));
    return;
  }

  // Build metadata from options or environment variables.
  const name =
    options.name ||
    process.env.CLEAT_WORKFLOW_NAME ||
    "unknown";
  const version =
    options.version !== undefined
      ? options.version
      : process.env.CLEAT_WORKFLOW_VERSION !== undefined
        ? parseInt(process.env.CLEAT_WORKFLOW_VERSION, 10)
        : 0;
  const minVersion =
    options.minVersion !== undefined
      ? options.minVersion
      : process.env.CLEAT_MIN_COMPATIBLE_VERSION !== undefined
        ? parseInt(process.env.CLEAT_MIN_COMPATIBLE_VERSION, 10)
        : 1;
  const abiVersion =
    options.abiVersion !== undefined
      ? options.abiVersion
      : process.env.CLEAT_ABI_VERSION !== undefined
        ? parseInt(process.env.CLEAT_ABI_VERSION, 10)
        : 1;
  const pluginDepsStr =
    options.pluginDeps || process.env.CLEAT_PLUGIN_DEPS || "{}";

  let pluginDeps = {};
  try {
    pluginDeps = JSON.parse(pluginDepsStr);
    if (typeof pluginDeps !== "object" || Array.isArray(pluginDeps)) {
      pluginDeps = {};
    }
  } catch {
    pluginDeps = {};
  }

  const meta = {
    workflow_name: name,
    workflow_version: version,
    min_compatible_version: minVersion,
    abi_version: abiVersion,
    plugin_deps: pluginDeps,
    sdk_language: "assemblyscript",
    sdk_version: "0.1.0",
    created_at: new Date().toISOString(),
  };

  const modified = injectMetadata(wasmBytes, meta, wasmFile);
  const outputPath = options.output || wasmFile;
  fs.writeFileSync(outputPath, modified);

  const isVerbose = args.includes("--verbose") || args.includes("-v");
  if (isVerbose) {
    console.log(`Stamped metadata into ${outputPath}`);
    console.log(`  workflow_name:           ${meta.workflow_name}`);
    console.log(`  workflow_version:        ${meta.workflow_version}`);
    console.log(`  min_compatible_version:  ${meta.min_compatible_version}`);
    console.log(`  abi_version:             ${meta.abi_version}`);
    if (Object.keys(meta.plugin_deps).length > 0) {
      console.log(`  plugin_deps:             ${JSON.stringify(meta.plugin_deps)}`);
    }
    const origSize = wasmBytes.length;
    const newSize = modified.length;
    console.log(`  Size: ${origSize} bytes -> ${newSize} bytes (+${newSize - origSize})`);
  } else {
    console.log(`Stamped cleat metadata into ${outputPath}`);
  }
}

if (require.main === module) {
  main();
}

module.exports = { injectMetadata, readMetadata, findCustomSection, buildCustomSection };
