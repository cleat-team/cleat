// The `plugins` group of host-call bindings (cleat#3827). Empty until the PR that binds this group's calls lands.
//
// Put here: the `declare function __cleat_...` lines for the group's calls, the methods they add to `Ctx` (as members of
// `PluginsCtx`), and their implementations (in `makePlugins`). index.ts already extends `Ctx` with `PluginsCtx` and spreads
// `makePlugins(h)` into the context, so this is the only TypeScript file the group's PR needs to edit. Conventions:
// groups/README.md.

import type { Helpers } from "./helpers";

export interface PluginsCtx {}

export function makePlugins(_h: Helpers): PluginsCtx {
  return {};
}
