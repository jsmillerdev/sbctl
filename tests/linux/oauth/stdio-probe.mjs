// Stands in for internal/api/testdata/mcp-smoke.mjs and mcp-roles.mjs in selftest.mjs: it takes the
// OAuth access token from SUPABASE_ACCESS_TOKEN, as those scripts do, and checks that the fake node
// accepts it. FAKE_NODE_PORT names the node; the first argument (the API URL) is not used.
import http from "node:http";

const [, ref, role] = process.argv.slice(2);
const token = process.env.SUPABASE_ACCESS_TOKEN ?? "";
const status = await new Promise((resolve, reject) => {
  http
    .get({ host: "127.0.0.1", port: process.env.FAKE_NODE_PORT, path: "/v1/organizations", headers: { host: "api.supavise.test", authorization: `Bearer ${token}` } }, (res) => {
      res.resume();
      resolve(res.statusCode);
    })
    .on("error", reject);
});
console.error(`      stdio probe ${role ?? "smoke"} ${ref} -> ${status}`);
process.exit(status === 200 ? 0 : 1);
