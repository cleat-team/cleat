# Build-time version constants for cleat Python workflows.
#
# These values are set by the build system (Makefile or stamp_metadata.py)
# before WASM compilation. They are read by stamp_metadata.py to inject
# the "cleat.metadata" custom section into the compiled WASM binary.
#
# Default values ensure unconfigured builds still produce valid output.

# WORKFLOW_NAME is the human-readable name of this workflow definition.
WORKFLOW_NAME = "unknown"

# WORKFLOW_VERSION is the monotonic version number for this workflow def.
WORKFLOW_VERSION = 0

# MIN_COMPATIBLE_VERSION is the minimum compatible workflow definition version.
MIN_COMPATIBLE_VERSION = 1

# ABI_VERSION is the WASM host ABI version this module targets.
ABI_VERSION = 1

# PLUGIN_DEPS is a dict mapping plugin names to semver constraint strings.
# Example: {"llm": ">=1.2.0", "blobstore": "~2.0.0"}
PLUGIN_DEPS = {}

# CHILD_BINDING_POLICY is the child workflow version binding policy.
# Values: "", "frozen", "stable", "latest", or "tag:<name>"
CHILD_BINDING_POLICY = ""

# SDK_LANGUAGE identifies the SDK that produced this module.
SDK_LANGUAGE = "python"

# SDK_VERSION is the version of the cleat Python SDK.
SDK_VERSION = "0.2.0"
