// Package functionsmain embeds the Deno main service of the Edge Runtime (the TypeScript
// next to this file) so the sbctl binary and the main service it starts always match.
// internal/fleet writes the files under <state_dir>/system/edge-runtime/main before it
// starts sb-edge-runtime; tests and deno.json stay out of the binary.
package functionsmain

import "embed"

// Files holds index.ts and the modules it imports, nothing else.
//
//go:embed index.ts src/auth.ts src/body.ts src/config.ts src/handler.ts src/limiter.ts src/projects.ts src/runtime.ts src/types.ts
var Files embed.FS
