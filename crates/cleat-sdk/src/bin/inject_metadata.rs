//! inject_metadata — Post-compile metadata stamping for Rust WASM workflows.
//!
//! Reads a compiled WASM binary (produced by `cargo build --target wasm32-wasip1`)
//! and injects a "cleat.metadata" custom section containing workflow identity
//! information. The host can read this metadata without instantiating the module.
//!
//! Usage:
//!   cargo run --bin inject_metadata -- <wasm-file> \
//!       --name PlaceOrder \
//!       --version 3 \
//!       [--output <output.wasm>]
//!
//! Environment variables are also supported:
//!   CLEAT_WORKFLOW_NAME, CLEAT_WORKFLOW_VERSION, CLEAT_MIN_COMPATIBLE_VERSION,
//!   CLEAT_ABI_VERSION, CLEAT_PLUGIN_DEPS

use std::env;
use std::fs;

fn main() {
    let args: Vec<String> = env::args().collect();

    if args.len() < 2 || args.contains(&"--help".to_string()) || args.contains(&"-h".to_string()) {
        eprintln!(
            r#"Usage: inject_metadata <wasm-file> [options]

Options:
  --name <name>                 Workflow name (or CLEAT_WORKFLOW_NAME env var)
  --version <n>                 Workflow version (or CLEAT_WORKFLOW_VERSION env var)
  --min-version <n>             Min compatible version (or CLEAT_MIN_COMPATIBLE_VERSION env var)
  --abi-version <n>             ABI version (or CLEAT_ABI_VERSION env var)
  --plugin-deps <json>          Plugin dependencies JSON (or CLEAT_PLUGIN_DEPS env var)
  --child-binding-policy <str>  Child binding policy (or CLEAT_CHILD_BINDING_POLICY env var)
  --output, -o <file>           Output WASM path (default: overwrite input)
  --read                        Read and display metadata instead of writing
"#
        );
        std::process::exit(0);
    }

    let read_only = args.contains(&"--read".to_string());

    let mut wasm_file = None;
    let mut output_file = None;
    let mut name = None;
    let mut version = None;
    let mut min_version = None;
    let mut abi_version = None;
    let mut plugin_deps = None;
    let mut child_binding_policy: Option<String> = None;

    let mut i = 1;
    while i < args.len() {
        match args[i].as_str() {
            "--output" | "-o" => {
                i += 1;
                output_file = Some(args[i].clone());
            }
            "--name" => {
                i += 1;
                name = Some(args[i].clone());
            }
            "--version" => {
                i += 1;
                version = Some(args[i].parse::<u32>().expect("version must be a number"));
            }
            "--min-version" => {
                i += 1;
                min_version = Some(args[i].parse::<u32>().expect("min-version must be a number"));
            }
            "--abi-version" => {
                i += 1;
                abi_version = Some(args[i].parse::<u32>().expect("abi-version must be a number"));
            }
            "--plugin-deps" => {
                i += 1;
                plugin_deps = Some(args[i].clone());
            }
            "--child-binding-policy" => {
                i += 1;
                child_binding_policy = Some(args[i].clone());
            }
            "--read" => {}
            s if !s.starts_with("--") => {
                wasm_file = Some(args[i].clone());
            }
            _ => {
                eprintln!("Unknown option: {}", args[i]);
                std::process::exit(1);
            }
        }
        i += 1;
    }

    let wasm_file = wasm_file.expect("<wasm-file> is required");
    let wasm_bytes = match fs::read(&wasm_file) {
        Ok(bytes) => bytes,
        Err(e) => {
            eprintln!("Failed to read WASM file {}: {}", wasm_file, e);
            std::process::exit(1);
        }
    };

    if read_only {
        match read_metadata(&wasm_bytes) {
            Some(meta) => {
                println!("{}", serde_json::to_string_pretty(&meta).unwrap());
            }
            None => {
                eprintln!("No 'cleat.metadata' section found in {}", wasm_file);
                std::process::exit(1);
            }
        }
        return;
    }

    // Build metadata from args or environment variables.
    let resolved_name = name
        .or_else(|| env::var("CLEAT_WORKFLOW_NAME").ok())
        .unwrap_or_else(|| "unknown".to_string());

    let resolved_version = version
        .or_else(|| {
            env::var("CLEAT_WORKFLOW_VERSION")
                .ok()
                .map(|v| v.parse::<u32>().unwrap_or(0))
        })
        .unwrap_or(0);

    let resolved_min_version = min_version
        .or_else(|| {
            env::var("CLEAT_MIN_COMPATIBLE_VERSION")
                .ok()
                .map(|v| v.parse::<u32>().unwrap_or(1))
        })
        .unwrap_or(1);

    let resolved_abi_version = abi_version
        .or_else(|| {
            env::var("CLEAT_ABI_VERSION")
                .ok()
                .map(|v| v.parse::<u32>().unwrap_or(1))
        })
        .unwrap_or(1);

    let deps_str = plugin_deps
        .or_else(|| env::var("CLEAT_PLUGIN_DEPS").ok())
        .unwrap_or_else(|| "{}".to_string());

    let deps_map: std::collections::HashMap<String, String> =
        serde_json::from_str(&deps_str).unwrap_or_default();

    let resolved_child_binding_policy = child_binding_policy
        .or_else(|| env::var("CLEAT_CHILD_BINDING_POLICY").ok())
        .unwrap_or_else(|| "".to_string());

    let meta = serde_json::json!({
        "workflow_name": resolved_name,
        "workflow_version": resolved_version,
        "min_compatible_version": resolved_min_version,
        "abi_version": resolved_abi_version,
        "plugin_deps": deps_map,
        "child_binding_policy": resolved_child_binding_policy,
        "sdk_language": "rust",
        "language": "rust",
        "sdk_version": "0.1.0",
        "created_at": chrono_now(),
    });

    let modified = inject_metadata(&wasm_bytes, &meta);
    let output_path = output_file.unwrap_or(wasm_file);
    match fs::write(&output_path, modified) {
        Ok(()) => {}
        Err(e) => {
            eprintln!("Failed to write output to {}: {}", output_path, e);
            std::process::exit(1);
        }
    }

    eprintln!("Stamped cleat metadata into {}", output_path);
}

fn chrono_now() -> String {
    // Use a simple UTC timestamp without depending on the `chrono` crate.
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default();
    let secs = now.as_secs();
    // Format as ISO 8601
    let days = secs / 86400;
    let time_secs = secs % 86400;
    let hours = time_secs / 3600;
    let minutes = (time_secs % 3600) / 60;
    let seconds = time_secs % 60;

    // Simple date calculation from Unix epoch (1970-01-01).
    let (year, month, day) = days_to_date(days as i64);
    format!(
        "{:04}-{:02}-{:02}T{:02}:{:02}:{:02}Z",
        year, month, day, hours, minutes, seconds
    )
}

fn days_to_date(mut days: i64) -> (i64, i64, i64) {
    // Algorithm from http://howardhinnant.github.io/date_algorithms.html
    days += 719468;
    let era = if days >= 0 { days } else { days - 146096 } / 146097;
    let doe = days - era * 146097;
    let yoe = (doe - doe / 1460 + doe / 36524 - doe / 146096) / 365;
    let y = yoe + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = doy - (153 * mp + 2) / 5 + 1;
    let m = if mp < 10 { mp + 3 } else { mp - 9 };
    let y = if m <= 2 { y + 1 } else { y };
    (y, m, d)
}

// ---- WASM binary manipulation ----

const SECTION_NAME: &str = "cleat.metadata";
const WASM_MAGIC: [u8; 4] = [0x00, 0x61, 0x73, 0x6D]; // \0asm
const WASM_VERSION: [u8; 4] = [0x01, 0x00, 0x00, 0x00];

fn encode_uleb128(mut value: u64) -> Vec<u8> {
    let mut result = Vec::new();
    loop {
        let mut b = (value & 0x7F) as u8;
        value >>= 7;
        if value != 0 {
            b |= 0x80;
        }
        result.push(b);
        if value == 0 {
            break;
        }
    }
    result
}

fn decode_uleb128(data: &[u8], offset: usize) -> Option<(u64, usize)> {
    let mut result: u64 = 0;
    let mut shift: u32 = 0;
    let mut consumed = 0;

    for &b in data.iter().skip(offset) {
        consumed += 1;
        result |= ((b & 0x7F) as u64) << shift;
        if b & 0x80 == 0 {
            return Some((result, consumed));
        }
        shift += 7;
        if shift > 63 {
            return None;
        }
    }
    None
}

fn build_custom_section(name: &str, payload: &[u8]) -> Vec<u8> {
    let name_bytes = name.as_bytes();
    let name_len = encode_uleb128(name_bytes.len() as u64);
    let mut full_payload = Vec::new();
    full_payload.extend_from_slice(&name_len);
    full_payload.extend_from_slice(name_bytes);
    full_payload.extend_from_slice(payload);

    let payload_len = encode_uleb128(full_payload.len() as u64);
    let mut section = vec![0x00]; // section ID 0 (custom)
    section.extend_from_slice(&payload_len);
    section.extend_from_slice(&full_payload);
    section
}

fn find_custom_section(wasm_bytes: &[u8], name: &str) -> Option<(Vec<u8>, usize, usize)> {
    if wasm_bytes.len() < 8 {
        return None;
    }
    if wasm_bytes[..4] != WASM_MAGIC || wasm_bytes[4..8] != WASM_VERSION {
        return None;
    }

    let name_bytes = name.as_bytes();
    let mut offset = 8;

    while offset < wasm_bytes.len() {
        let section_start = offset;
        let section_id = wasm_bytes[offset];
        offset += 1;

        if offset >= wasm_bytes.len() {
            break;
        }

        let (payload_len, consumed) = decode_uleb128(wasm_bytes, offset)?;
        offset += consumed;
        let section_end = offset + payload_len as usize;

        if section_end > wasm_bytes.len() {
            return None;
        }

        if section_id == 0 {
            // Custom section — check name.
            if let Some((name_len, nc)) = decode_uleb128(wasm_bytes, offset) {
                let name_start = offset + nc;
                let name_end = name_start + name_len as usize;
                if name_end <= section_end && &wasm_bytes[name_start..name_end] == name_bytes {
                    return Some((
                        wasm_bytes[name_end..section_end].to_vec(),
                        section_start,
                        section_end,
                    ));
                }
            }
        }

        offset += payload_len as usize;
    }

    None
}

fn inject_metadata(wasm_bytes: &[u8], meta: &serde_json::Value) -> Vec<u8> {
    let json_payload = serde_json::to_string(meta).unwrap();
    let json_bytes = json_payload.as_bytes();

    // Remove existing section if present.
    let base = match find_custom_section(wasm_bytes, SECTION_NAME) {
        Some((_, start, end)) => {
            let mut v = Vec::with_capacity(wasm_bytes.len() - (end - start));
            v.extend_from_slice(&wasm_bytes[..start]);
            v.extend_from_slice(&wasm_bytes[end..]);
            v
        }
        None => wasm_bytes.to_vec(),
    };

    let new_section = build_custom_section(SECTION_NAME, json_bytes);
    let mut result = base;
    result.extend_from_slice(&new_section);
    result
}

fn read_metadata(wasm_bytes: &[u8]) -> Option<serde_json::Value> {
    let (payload, _, _) = find_custom_section(wasm_bytes, SECTION_NAME)?;
    serde_json::from_slice(&payload).ok()
}
