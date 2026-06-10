fn main() {
    // Workflow metadata — read from environment variables at build time.
    // Falls back to defaults so unconfigured builds still succeed.

    let workflow_name = std::env::var("CLEAT_WORKFLOW_NAME")
        .unwrap_or_else(|_| "unknown".to_string());
    println!("cargo:rustc-env=CLEAT_WORKFLOW_NAME={}", workflow_name);

    let workflow_version = std::env::var("CLEAT_WORKFLOW_VERSION")
        .unwrap_or_else(|_| "1".to_string());
    println!("cargo:rustc-env=CLEAT_WORKFLOW_VERSION={}", workflow_version);

    let min_compatible_version = std::env::var("CLEAT_MIN_COMPATIBLE_VERSION")
        .unwrap_or_else(|_| "1".to_string());
    println!("cargo:rustc-env=CLEAT_MIN_COMPATIBLE_VERSION={}", min_compatible_version);

    let abi_version = std::env::var("CLEAT_ABI_VERSION")
        .unwrap_or_else(|_| "1".to_string());
    println!("cargo:rustc-env=CLEAT_ABI_VERSION={}", abi_version);

    let plugin_deps = std::env::var("CLEAT_PLUGIN_DEPS")
        .unwrap_or_else(|_| "{}".to_string());
    println!("cargo:rustc-env=CLEAT_PLUGIN_DEPS={}", plugin_deps);

    let child_binding_policy = std::env::var("CLEAT_CHILD_BINDING_POLICY")
        .unwrap_or_else(|_| "".to_string());
    println!("cargo:rustc-env=CLEAT_CHILD_BINDING_POLICY={}", child_binding_policy);

    // Re-run build.rs if any of these env vars change
    println!("cargo:rerun-if-env-changed=CLEAT_WORKFLOW_NAME");
    println!("cargo:rerun-if-env-changed=CLEAT_WORKFLOW_VERSION");
    println!("cargo:rerun-if-env-changed=CLEAT_MIN_COMPATIBLE_VERSION");
    println!("cargo:rerun-if-env-changed=CLEAT_ABI_VERSION");
    println!("cargo:rerun-if-env-changed=CLEAT_PLUGIN_DEPS");
    println!("cargo:rerun-if-env-changed=CLEAT_CHILD_BINDING_POLICY");
}
