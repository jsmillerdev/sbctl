// Smoke test of the Supabase MCP server (stdio) against a Management API.
//
//   SUPABASE_ACCESS_TOKEN=<pat> node mcp-smoke.mjs <api-url> <project-ref>
//
// The token travels in the environment, never in argv, where any local user could
// read it from the process list.
//
// Starts @supabase/mcp-server-supabase with --api-url, speaks newline-delimited
// JSON-RPC to it and calls the tools sbctl's API must serve. Exits non-zero when a
// call fails. No dependencies: the MCP stdio transport is one JSON message per line.
import { spawn } from "node:child_process";
import { createInterface } from "node:readline";

const [apiUrl, ref] = process.argv.slice(2);
const pat = process.env.SUPABASE_ACCESS_TOKEN;
if (!apiUrl || !pat || !ref) {
  console.error("usage: SUPABASE_ACCESS_TOKEN=<pat> node mcp-smoke.mjs <api-url> <project-ref>");
  process.exit(2);
}
const version = process.env.MCP_VERSION || "latest";
let failed = 0;
const text = (r) => (r.result?.content ?? []).map((c) => c.text ?? "").join("\n");

// session starts the MCP server (scoped to a project when projectRef is given) and
// runs fn with a tool-calling helper.
async function session(label, projectRef, fn) {
  console.log(`--- ${label}`);
  const args = ["-y", `@supabase/mcp-server-supabase@${version}`, "--api-url", apiUrl];
  if (projectRef) args.push("--project-ref", projectRef);
  const child = spawn("npx", args, { stdio: ["pipe", "pipe", "inherit"], env: { ...process.env, SUPABASE_ACCESS_TOKEN: pat } });
  const pending = new Map();
  let nextId = 1;
  createInterface({ input: child.stdout }).on("line", (line) => {
    let msg;
    try { msg = JSON.parse(line); } catch { return; }
    if (msg.id !== undefined && pending.has(msg.id)) {
      pending.get(msg.id)(msg);
      pending.delete(msg.id);
    }
  });
  const send = (m) => child.stdin.write(JSON.stringify(m) + "\n");
  const rpc = (method, params) =>
    new Promise((resolve, reject) => {
      const id = nextId++;
      const timer = setTimeout(() => reject(new Error(`${method} timed out`)), 90000);
      pending.set(id, (m) => { clearTimeout(timer); resolve(m); });
      send({ jsonrpc: "2.0", id, method, params });
    });
  const init = await rpc("initialize", {
    protocolVersion: "2025-06-18",
    capabilities: {},
    clientInfo: { name: "sbctl-smoke", version: "0" },
  });
  if (init.error) { console.error(JSON.stringify(init.error)); process.exit(1); }
  send({ jsonrpc: "2.0", method: "notifications/initialized" });
  const tools = (await rpc("tools/list", {})).result?.tools?.map((t) => t.name) ?? [];
  console.log(`tools: ${tools.join(", ")}`);
  async function tool(name, args = {}, check = () => true) {
    const r = await rpc("tools/call", { name, arguments: args });
    const out = text(r);
    const ok = !r.error && !r.result?.isError && check(out);
    if (!ok) failed++;
    console.log(`${ok ? "PASS" : "FAIL"} ${name} ${JSON.stringify(args).slice(0, 80)} -> ${(r.error ? JSON.stringify(r.error) : out).replace(/\s+/g, " ").slice(0, 200)}`);
    return out;
  }
  try {
    await fn({ tool, rpc, tools });
  } finally {
    child.kill();
  }
}

await session("project-scoped (database, development, debugging)", ref, async ({ tool, rpc, tools }) => {
  await tool("execute_sql", { query: "create table if not exists public.mcp_widgets (id bigint primary key, name text); insert into public.mcp_widgets values (1, 'a') on conflict do nothing" });
  await tool("list_tables", { schemas: ["public"] }, (o) => o.includes("mcp_widgets"));
  await tool("execute_sql", { query: "select id, name from public.mcp_widgets" }, (o) => o.includes("mcp_widgets") === false && /name\\?":\s*\\?"a/.test(o));
  await tool("list_extensions", {}, (o) => o.includes("plpgsql"));
  await tool("apply_migration", { name: "mcp_smoke", query: "create table public.mcp_migrated (id int)" });
  await tool("list_migrations", {}, (o) => o.includes("mcp_smoke"));
  await tool("generate_typescript_types", {}, (o) => o.includes("mcp_widgets"));
  // Known upstream limitation (research/05 section 3.3): the URL is derived from the API host.
  await tool("get_project_url", {});
  await tool("get_publishable_keys", {}, (o) => o.includes("sb_publishable_"));
  await tool("list_edge_functions", {});
  await tool("get_advisors", { type: "security" });
  const bad = await rpc("tools/call", { name: "execute_sql", arguments: { query: "select * from does_not_exist" } });
  const badOk = bad.result?.isError || bad.error;
  console.log(`${badOk ? "PASS" : "FAIL"} execute_sql error surfaced -> ${(text(bad) || JSON.stringify(bad.error)).replace(/\s+/g, " ").slice(0, 160)}`);
  if (!badOk) failed++;
});

await session("account-scoped (no project ref)", "", async ({ tool }) => {
  await tool("list_organizations", {}, (o) => o.includes("default"));
  await tool("get_organization", { id: "1" }, (o) => o.includes("Default") || o.includes("default"));
  await tool("list_projects", {}, (o) => o.includes(ref));
  await tool("get_project", { id: ref }, (o) => o.includes(ref));
});

console.log(failed ? `${failed} failure(s)` : "all MCP checks passed");
process.exit(failed ? 1 : 0);
