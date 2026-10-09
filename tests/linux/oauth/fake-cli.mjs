// The operator CLI as fake-node.mjs understands it: `users invite|remove` and `oauth grants list|revoke`.
// selftest.mjs puts it in the walk's "cli" prefix; FAKE_NODE_PORT names the fake node.
import http from "node:http";

const args = process.argv.slice(2);
const flag = (name) => {
  const i = args.indexOf(name);
  return i >= 0 ? args[i + 1] : undefined;
};
const [a, b, c] = args;
let path;
const q = new URLSearchParams();
if (a === "users" && b === "invite") {
  path = "/__cli/invite";
  q.set("email", c);
  q.set("role", flag("--role"));
} else if (a === "users" && b === "remove") {
  path = "/__cli/remove";
  q.set("email", c);
} else if (a === "oauth" && b === "grants" && (c === "list" || c === "revoke")) {
  path = "/__cli/grants";
  if (flag("--user")) q.set("user", flag("--user"));
  if (flag("--app")) q.set("app", flag("--app"));
  if (c === "revoke") {
    if (!args.includes("--yes")) {
      console.error("refusing without --yes");
      process.exit(1);
    }
    q.set("revoke", "1");
  }
} else {
  console.error("fake-cli: unknown command " + args.join(" "));
  process.exit(2);
}
http.get({ host: "127.0.0.1", port: process.env.FAKE_NODE_PORT, path: `${path}?${q}` }, (res) => {
  let s = "";
  res.on("data", (d) => (s += d));
  res.on("end", () => {
    (res.statusCode === 200 ? process.stdout : process.stderr).write(s);
    process.exit(res.statusCode === 200 ? 0 : 1);
  });
});
