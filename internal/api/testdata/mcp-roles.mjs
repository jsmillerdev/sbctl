// The Supabase MCP server (stdio) against a Management API, as a user with one role.
//
//   SUPABASE_ACCESS_TOKEN=<pat> node mcp-roles.mjs <api-url> <project-ref> <owner|read-only>
//
// "owner" must be able to write (execute_sql, apply_migration); "read-only" must be able to read
// everything and be refused every write, with the database's own error for SQL. The token
// travels in the environment. Exits non-zero when an expectation fails.
import { spawn } from "node:child_process";
import { createInterface } from "node:readline";

const [apiUrl, ref, role] = process.argv.slice(2);
const pat = process.env.SUPABASE_ACCESS_TOKEN;
if (!apiUrl || !pat || !ref || !["owner", "read-only"].includes(role)) {
  console.error("usage: SUPABASE_ACCESS_TOKEN=<pat> node mcp-roles.mjs <api-url> <project-ref> <owner|read-only>");
  process.exit(2);
}
const readOnly = role === "read-only";
const version = process.env.MCP_VERSION || "latest";
let failed = 0;
const text = (r) => (r.result?.content ?? []).map((c) => c.text ?? "").join("\n");

const child = spawn("npx", ["-y", `@supabase/mcp-server-supabase@${version}`, "--api-url", apiUrl, "--project-ref", ref],
  { stdio: ["pipe", "pipe", "inherit"], env: { ...process.env, SUPABASE_ACCESS_TOKEN: pat } });
const pending = new Map();
let nextId = 1;
createInterface({ input: child.stdout }).on("line", (line) => {
  let msg;
  try { msg = JSON.parse(line); } catch { return; }
  if (msg.id !== undefined && pending.has(msg.id)) { pending.get(msg.id)(msg); pending.delete(msg.id); }
});
const send = (m) => child.stdin.write(JSON.stringify(m) + "\n");
const rpc = (method, params) => new Promise((resolve, reject) => {
  const id = nextId++;
  const timer = setTimeout(() => reject(new Error(`${method} timed out`)), 90000);
  pending.set(id, (m) => { clearTimeout(timer); resolve(m); });
  send({ jsonrpc: "2.0", id, method, params });
});

// call runs a tool; want is "ok" (must succeed) or "refused" (must fail); check inspects the text.
async function call(name, args, want, check = () => true) {
  const r = await rpc("tools/call", { name, arguments: args });
  const out = text(r) || JSON.stringify(r.error ?? "");
  const failedCall = Boolean(r.error || r.result?.isError);
  const ok = want === "ok" ? !failedCall && check(out) : failedCall && check(out);
  if (!ok) failed++;
  console.log(`${ok ? "PASS" : "FAIL"} [${role}] ${name} ${JSON.stringify(args).slice(0, 70)} (${want}) -> ${out.replace(/\s+/g, " ").slice(0, 160)}`);
}

try {
  const init = await rpc("initialize", { protocolVersion: "2025-06-18", capabilities: {}, clientInfo: { name: "supavise-roles", version: "0" } });
  if (init.error) { console.error(JSON.stringify(init.error)); process.exit(1); }
  send({ jsonrpc: "2.0", method: "notifications/initialized" });
  const write = readOnly ? "refused" : "ok";
  if (!readOnly) {
    await call("execute_sql", { query: "create table if not exists public.roles_widgets (id bigint primary key, name text); insert into public.roles_widgets values (1, 'a') on conflict do nothing" }, "ok");
  }
  // Reads, for every role.
  await call("list_tables", { schemas: ["public"] }, "ok", (o) => o.includes("roles_widgets"));
  await call("execute_sql", { query: "select id, name from public.roles_widgets" }, "ok", (o) => /name\\?":\s*\\?"a/.test(o));
  await call("list_migrations", {}, "ok");
  await call("generate_typescript_types", {}, "ok", (o) => o.includes("roles_widgets"));
  await call("get_advisors", { type: "security" }, "ok");
  await call("list_extensions", {}, "ok", (o) => o.includes("plpgsql"));
  // Writes, for the roles that have them.
  await call("execute_sql", { query: "insert into public.roles_widgets values (2, 'b')" }, write, readOnly ? (o) => /read-only|permission denied/i.test(o) : undefined);
  await call("apply_migration", { name: `roles_${role.replace("-", "_")}`, query: "create table public.roles_migrated_" + role.replace("-", "_") + " (id int)" }, write);
  // Nothing the read-only role tried changed the schema.
  await call("list_tables", { schemas: ["public"] }, "ok", (o) => o.includes("roles_widgets"));
} finally {
  child.kill();
}
console.log(failed ? `${failed} failure(s)` : "all MCP role checks passed");
process.exit(failed ? 1 : 0);
