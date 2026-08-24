// zync integration for opencode.
//
// Install (global): ln -s <this file> ~/.config/opencode/plugins/zync.js
//
// Behavior:
//   - Before any mutating tool runs, ensure this replica holds the zync
//     write lease (`zx take`). If it's free, it is acquired automatically;
//     if another replica holds it, the tool call is blocked with a message
//     saying who has it.
//   - When the session goes idle and ZYNC_AUTO_HANDOFF=1 is set, flush and
//     release (`zx handoff`) so other replicas can pick the work up. Set
//     this on server-side agents; leave it unset on your laptop so you keep
//     the lease for manual editing after a session.
//
// Repos not enrolled in zync (no .git/zync-state.json) are ignored entirely.
import { existsSync } from "node:fs"
import { join } from "node:path"

const MUTATING_TOOLS = new Set(["write", "edit", "multiedit", "patch", "bash"])

export const Zync = async ({ directory, worktree, $ }) => {
  const dir = worktree || directory
  const enrolled = existsSync(join(dir, ".git", "zync-state.json"))
  if (!enrolled) return {}

  const autoHandoff = process.env.ZYNC_AUTO_HANDOFF === "1"
  const zx = (...args) => $`zx ${args}`.cwd(dir).quiet()

  return {
    "tool.execute.before": async (input) => {
      if (!MUTATING_TOOLS.has(input.tool)) return
      try {
        await zx("take")
      } catch (err) {
        const detail = err?.stderr?.toString?.() || err?.message || String(err)
        throw new Error(
          `zync: this machine does not hold the write lease for this repo, ` +
            `so edits are blocked. ${detail.trim()} ` +
            `Run \`zx take --force\` yourself if you want to break the lease.`,
        )
      }
    },

    event: async ({ event }) => {
      if (event.type === "session.idle" && autoHandoff) {
        try {
          await zx("handoff")
        } catch {
          // Not holding (or flush failed and the lease was retained):
          // nothing to release, or safer to keep it. Never crash the session.
        }
      }
    },
  }
}
