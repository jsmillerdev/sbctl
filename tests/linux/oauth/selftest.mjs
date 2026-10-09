// Runs oauth-walk.mjs against fake-node.mjs on a development machine, with no root, no systemd and no
// Postgres:
//
//   node tests/linux/oauth/selftest.mjs [--mcp]
//
// It checks the walk against the contract of docs/design.md section 13, not the server against the walk. A failure here
// is a bug in the walk or in the fake; a failure in oauth-smoke.sh after this passes is a difference
// between the server and the design. A pass says nothing about the real edge (Host dispatch, the mux
// fallback, the headers Studio's consent pages carry): the fake copies what the code does today, and
// only oauth-smoke checks the node.
import { spawn } from "node:child_process";
import { mkdtempSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { startFake } from "./fake-node.mjs";

const here = dirname(fileURLToPath(import.meta.url));
const mcp = process.argv.includes("--mcp");
const node = await startFake({ mcp });
const dir = mkdtempSync(join(tmpdir(), "oauth-walk-"));
const cfgPath = join(dir, "config.json");
writeFileSync(
  cfgPath,
  JSON.stringify({
    domain: node.domain,
    issuer: node.issuer,
    dashboard: node.dashboard,
    proxy: { host: "127.0.0.1", port: node.port },
    admin_url: `http://127.0.0.1:${node.adminPort}`,
    org: node.org,
    org2: node.org2,
    ref: node.ref,
    ref2: node.ref2,
    owner: node.owner,
    owner_pat: node.pat,
    password: "walk-password-" + Date.now(),
    cli: [process.execPath, join(here, "fake-cli.mjs")],
    stdio: { smoke: join(here, "stdio-probe.mjs"), roles: join(here, "stdio-probe.mjs") },
    mcp,
  }),
  { mode: 0o600 },
);
const code = await new Promise((resolve) => {
  const child = spawn(process.execPath, [join(here, "oauth-walk.mjs"), cfgPath], {
    stdio: "inherit",
    env: { ...process.env, FAKE_NODE_PORT: String(node.port) },
  });
  child.on("exit", resolve);
});
await node.close();
rmSync(dir, { recursive: true, force: true });
console.error(code === 0 ? "selftest: the walk passed against the fake node" : `selftest: the walk failed (exit ${code})`);
process.exit(code ?? 1);
