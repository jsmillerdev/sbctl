// Drives the Supabase MCP server's branch tools against a Management API (supavise).
//
//   SUPABASE_ACCESS_TOKEN=<pat> node mcp-branches.mjs <api-url> <parent-project-ref>
//
// Starts @supabase/mcp-server-supabase (stdio) with --api-url and calls, in order: get_cost and
// confirm_cost, create_branch, list_branches, execute_sql on the branch, apply_migration on the
// branch, merge_branch, rebase_branch, reset_branch and delete_branch. Exits non-zero on the
// first check that fails. The token travels in the environment, never in argv.
import { spawn } from "node:child_process";
import { createInterface } from "node:readline";

const [apiUrl, ref] = process.argv.slice(2);
const pat = process.env.SUPABASE_ACCESS_TOKEN;
if (!apiUrl || !pat || !ref) {
  console.error("usage: SUPABASE_ACCESS_TOKEN=<pat> node mcp-branches.mjs <api-url> <parent-project-ref>");
  process.exit(2);
}
const version = process.env.MCP_VERSION || "latest";
// Account scope (no --project-ref): the cost tools exist there, and every call names its project.
const args = ["-y", `@supabase/mcp-server-supabase@${version}`, "--api-url", apiUrl];
const child = spawn("npx", args, { stdio: ["pipe", "pipe", "inherit"], env: { ...process.env, SUPABASE_ACCESS_TOKEN: pat } });
const pending = new Map();
let nextId = 1;
let failed = 0;
createInterface({ input: child.stdout }).on("line", (line) => {
  let msg;
  try { msg = JSON.parse(line); } catch { return; }
  if (msg.id !== undefined && pending.has(msg.id)) { pending.get(msg.id)(msg); pending.delete(msg.id); }
});
const send = (m) => child.stdin.write(JSON.stringify(m) + "\n");
const rpc = (method, params, ms = 180000) => new Promise((resolve, reject) => {
  const id = nextId++;
  const timer = setTimeout(() => reject(new Error(`${method} timed out`)), ms);
  pending.set(id, (m) => { clearTimeout(timer); resolve(m); });
  send({ jsonrpc: "2.0", id, method, params });
});
const text = (r) => (r.result?.content ?? []).map((c) => c.text ?? "").join("\n");
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function tool(name, a = {}, check = () => true) {
  const t0 = Date.now();
  const r = await rpc("tools/call", { name, arguments: a });
  const out = text(r);
  const ok = !r.error && !r.result?.isError && check(out);
  if (!ok) failed++;
  console.log(`${ok ? "PASS" : "FAIL"} ${name} ${JSON.stringify(a).slice(0, 100)} (${Date.now() - t0} ms) -> ${(r.error ? JSON.stringify(r.error) : out).replace(/\s+/g, " ").slice(0, 260)}`);
  return { out, ok, raw: r };
}

// The MCP server wraps results in an "untrusted data" envelope; pull the JSON out of it.
function json(out) {
  const m = out.match(/(\[|\{)[\s\S]*(\]|\})/);
  try { return JSON.parse(m[0]); } catch { return null; }
}

async function branchStatus(name, want, timeoutMs = 180000) {
  const t0 = Date.now();
  for (;;) {
    const { out } = await tool("list_branches", { project_id: ref }, () => true);
    const list = json(out)?.branches ?? [];
    const b = list.find((x) => x.name === name);
    if (b && b.status === want) return b;
    // A failed operation ends in MIGRATIONS_FAILED: that is a failure of the run, not a state to
    // accept as "done" (unless the caller waits for it).
    if (b && b.status === "MIGRATIONS_FAILED") {
      failed++;
      console.log(`FAIL branch ${name} ended MIGRATIONS_FAILED while waiting for ${want}`);
      return b;
    }
    if (Date.now() - t0 > timeoutMs) { failed++; console.log(`FAIL branch ${name} never reached ${want}`); return b; }
    await sleep(1500);
  }
}

try {
  const init = await rpc("initialize", { protocolVersion: "2025-06-18", capabilities: {}, clientInfo: { name: "supavise-branches", version: "0" } });
  if (init.error) { console.error(JSON.stringify(init.error)); process.exit(1); }
  send({ jsonrpc: "2.0", method: "notifications/initialized" });
  const tools = (await rpc("tools/list", {})).result?.tools?.map((t) => t.name) ?? [];
  console.log(`tools: ${tools.join(", ")}`);
  for (const t of ["create_branch", "list_branches", "merge_branch", "reset_branch", "rebase_branch", "delete_branch"]) {
    if (!tools.includes(t)) { failed++; console.log(`FAIL tool ${t} is not offered`); }
  }

  const orgs = json((await tool("list_organizations", {})).out)?.organizations ?? [];
  const org = orgs[0]?.id ?? "default";
  // The branch cost is a constant inside the MCP server; the confirmation id is derived from it.
  const cost = json((await tool("get_cost", { type: "branch", organization_id: org })).out) ?? {};
  const conf = json((await tool("confirm_cost", { type: "branch", recurrence: cost.recurrence, amount: cost.amount })).out) ?? {};

  const name = `mcp-${Date.now().toString(36)}`;
  await tool("create_branch", { project_id: ref, name, confirm_cost_id: conf.confirmation_id });
  const b = await branchStatus(name, "MIGRATIONS_PASSED");
  if (!b) throw new Error("no branch");
  console.log(`branch ${name}: ref=${b.project_ref} status=${b.status}`);

  // The branch is a project of its own: SQL and migrations run against its ref.
  await tool("execute_sql", { project_id: b.project_ref, query: "select count(*) as n from public.items" });
  await tool("apply_migration", { project_id: b.project_ref, name: "mcp_branch_change", query: "create table public.mcp_branch_table (id int primary key)" });
  await tool("list_migrations", { project_id: b.project_ref }, (o) => o.includes("mcp_branch_change"));
  await tool("list_tables", { project_id: ref, schemas: ["public"] }, (o) => !o.includes("mcp_branch_table"));

  await tool("merge_branch", { branch_id: b.id });
  await branchStatus(name, "MIGRATIONS_PASSED");
  await tool("list_tables", { project_id: ref, schemas: ["public"] }, (o) => o.includes("mcp_branch_table"));

  await tool("rebase_branch", { branch_id: b.id });
  await branchStatus(name, "MIGRATIONS_PASSED");

  await tool("execute_sql", { project_id: b.project_ref, query: "insert into public.items values (777777, 'scratch')" });
  await tool("reset_branch", { branch_id: b.id });
  await branchStatus(name, "MIGRATIONS_PASSED");
  await tool("execute_sql", { project_id: b.project_ref, query: "select count(*) as n from public.items where id = 777777" }, (o) => /"n"\s*:\s*0|\\"n\\":0/.test(o));

  await tool("delete_branch", { branch_id: b.id });
  const after = json((await tool("list_branches", { project_id: ref })).out)?.branches ?? [];
  if (after.some((x) => x.name === name)) { failed++; console.log("FAIL branch still listed after delete_branch"); }
} catch (e) {
  failed++;
  console.log(`FAIL ${e.message}`);
} finally {
  child.kill();
}
console.log(failed ? `${failed} failure(s)` : "all MCP branch checks passed");
process.exit(failed ? 1 : 0);
