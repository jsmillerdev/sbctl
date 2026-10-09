// The OAuth sign-in walk (steps L-01 to L-14; the contract is docs/design.md, section 13) against a running node.
//
//   node oauth-walk.mjs /path/to/walk-config.json
//   node oauth-walk.mjs --self-test
//
// tests/linux/oauth-smoke.sh starts the node, writes the config file (mode 0600) and runs this. The
// walk speaks plain HTTP to the node's proxy with a Host header (nothing needs DNS), signs in to the
// dashboard's GoTrue for the approvals, and drives the operator CLI through the command prefix in the
// config. Tokens, codes and passwords travel in headers and bodies, never on a command line, and the
// text of a failure has them cut out. No dependencies. Steps run in order and the first failure ends
// the walk. The steps that need Studio (L-09) run only when the config says "mcp": true.
//
// Config fields: domain, issuer, dashboard, proxy {host, port}, admin_url, org, org2, ref, ref2,
// owner {email, password}, owner_pat, password (for the users the walk creates), cli (argv prefix of
// the supavise command), mcp (bool), stdio {smoke, roles} (paths of the stdio MCP scripts, optional).
import crypto from "node:crypto";
import http from "node:http";
import { execFileSync, spawn } from "node:child_process";
import { readFileSync } from "node:fs";

// ---- helpers that need no node ---------------------------------------------------------------

const SECRET_RE = /\b(sbp_oauth_|sbp_|sbr_|sbc_|sba_|sbi_)[0-9a-zA-Z_]{8,}/g;
const JWT_RE = /\beyJ[\w-]+\.[\w-]+\.[\w-]+/g;
export const redact = (s) =>
  String(s)
    .replace(SECRET_RE, "$1<cut>")
    .replace(JWT_RE, "<jwt cut>")
    .replace(/([?&](?:code|code_challenge|refresh_token|access_token|client_secret|token)=)[^&\s"]+/g, "$1<cut>");

export const b64u = (buf) => Buffer.from(buf).toString("base64url");
export function pkcePair(verifier = b64u(crypto.randomBytes(32))) {
  return { verifier, challenge: b64u(crypto.createHash("sha256").update(verifier).digest()) };
}
export const basicHeader = (id, secret) =>
  "Basic " + Buffer.from(`${encodeURIComponent(id)}:${encodeURIComponent(secret)}`).toString("base64");

// sseMessages returns the JSON data of a text/event-stream body, one value per event.
export function sseMessages(body) {
  const out = [];
  for (const ev of body.split(/\r?\n\r?\n/)) {
    const data = ev.split(/\r?\n/).filter((l) => l.startsWith("data:")).map((l) => l.slice(5).trimStart()).join("\n");
    if (data) out.push(JSON.parse(data));
  }
  return out;
}

// canon sorts the keys of every object in v, so that a comparison sees values and not the order in which
// a server happened to write them. Arrays keep their order.
export const canon = (v) =>
  Array.isArray(v) ? v.map(canon) : v && typeof v === "object" ? Object.fromEntries(Object.keys(v).sort().map((k) => [k, canon(v[k])])) : v;

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const SCOPES13 = [
  "organizations:read", "projects:read", "projects:write", "database:read", "database:write",
  "analytics:read", "secrets:read", "edge_functions:read", "edge_functions:write",
  "environment:read", "environment:write", "storage:read", "storage:write",
];

if (process.argv[2] === "--self-test") {
  selfTest();
  process.exit(0);
}

function selfTest() {
  const must = (c, m) => { if (!c) { console.error("self-test failed: " + m); process.exit(1); } };
  // RFC 7636 appendix B.
  must(pkcePair("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk").challenge === "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM", "PKCE S256 vector");
  const p = pkcePair();
  must(p.verifier.length === 43 && /^[A-Za-z0-9_-]+$/.test(p.verifier), "verifier shape");
  // RFC 6749 section 2.3.1: the id and the secret are form-encoded before Basic.
  must(basicHeader("a b", "c:d") === "Basic " + Buffer.from("a%20b:c%3Ad").toString("base64"), "basic header encoding");
  const secret = "sbp_oauth_" + "a".repeat(40);
  must(!redact(`Bearer ${secret} x`).includes("aaaa"), "redact access token");
  must(!redact("http://x/cb?code=abc123&state=s").includes("abc123"), "redact code");
  must(!redact("eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2ln").includes("eyJzdWIi"), "redact jwt");
  const msgs = sseMessages('event: message\ndata: {"id":1,"result":{"a":1}}\n\nevent: message\ndata: {"id":2}\n\n');
  must(msgs.length === 2 && msgs[0].result.a === 1 && msgs[1].id === 2, "sse parse");
  must(UUID_RE.test(crypto.randomUUID()), "uuid shape");
  must(JSON.stringify(canon({ b: [{ d: 1, c: 2 }, 3], a: null })) === '{"a":null,"b":[{"c":2,"d":1},3]}', "canon sorts object keys and keeps array order");
  console.log("oauth-walk self-test ok");
}

// ---- configuration ---------------------------------------------------------------------------

const cfgPath = process.argv[2];
if (!cfgPath) {
  console.error("usage: node oauth-walk.mjs <config.json> | --self-test");
  process.exit(2);
}
const cfg = JSON.parse(readFileSync(cfgPath, "utf8"));
const { issuer, dashboard, org, org2, ref, ref2, domain } = cfg;
const apiHost = new URL(issuer).host;
const proxy = cfg.proxy ?? { host: "127.0.0.1", port: 80 };
const MCP_RESOURCE = `${issuer}/mcp`;
const PRM_URL = `${issuer}/.well-known/oauth-protected-resource/mcp`;
const LOOPBACK = "http://127.0.0.1:53682/callback";

const t0 = Date.now();
const log = (m) => console.error(`${String(Math.round((Date.now() - t0) / 1000)).padStart(4)}s ${m}`);

class WalkError extends Error {}
function ok(cond, what) {
  if (!cond) throw new WalkError(what);
}
function eq(got, want, what) {
  if (JSON.stringify(canon(got)) !== JSON.stringify(canon(want))) throw new WalkError(`${what}: got ${JSON.stringify(got)}, want ${JSON.stringify(want)}`);
}

async function step(id, title, fn) {
  const s = Date.now();
  try {
    await fn();
  } catch (e) {
    log(`${id} FAIL  ${title}`);
    console.error(redact(e instanceof WalkError ? e.message : e.stack ?? e));
    process.exit(1);
  }
  log(`${id} ok    ${title} (${Date.now() - s} ms)`);
}

// ---- HTTP ------------------------------------------------------------------------------------

class Res {
  constructor(status, headers, text, method, path) {
    Object.assign(this, { status, headers, text, method, path });
  }
  get json() {
    try {
      return JSON.parse(this.text);
    } catch {
      throw new WalkError(`${this.method} ${this.path} answered ${this.status} with a body that is not JSON: ${this.text.slice(0, 200)}`);
    }
  }
  h(name) {
    return this.headers[name.toLowerCase()];
  }
  describe() {
    return `${this.method} ${this.path} -> ${this.status} ${this.text.slice(0, 300)}`;
  }
}

function request(method, path, { headers = {}, body, host = apiHost, target = proxy, timeout = 180000 } = {}) {
  return new Promise((resolve, reject) => {
    const h = { host, ...headers };
    if (body !== undefined) h["content-length"] = Buffer.byteLength(body);
    // agent: false opens a connection per request, so a proxy that closes an idle one never meets a reused socket.
    const req = http.request({ host: target.host, port: target.port, method, path, headers: h, timeout, agent: false }, (res) => {
      const chunks = [];
      res.on("data", (c) => chunks.push(c));
      res.on("end", () => resolve(new Res(res.statusCode, res.headers, Buffer.concat(chunks).toString("utf8"), method, path)));
    });
    req.on("timeout", () => req.destroy(new Error(`${method} ${path} timed out`)));
    req.on("error", reject);
    if (body !== undefined) req.write(body);
    req.end();
  });
}

// call: token is a bearer; json and form are request bodies; headers add to the defaults.
function call(method, path, { token, json, form, headers = {}, ...rest } = {}) {
  const h = { accept: "application/json", ...headers };
  if (token) h.authorization = `Bearer ${token}`;
  let body;
  if (json !== undefined) {
    h["content-type"] = "application/json";
    body = JSON.stringify(json);
  } else if (form !== undefined) {
    h["content-type"] = "application/x-www-form-urlencoded";
    body = new URLSearchParams(form).toString();
  }
  return request(method, path, { headers: h, body, ...rest });
}

function status(res, want, what = "") {
  const wants = Array.isArray(want) ? want : [want];
  if (!wants.includes(res.status)) throw new WalkError(`${what ? what + ": " : ""}${res.describe()} (want ${wants.join(" or ")})`);
  return res;
}

// oauthError checks an RFC 6749 error answer: the status, the error code and the Supabase message key.
function oauthError(res, httpStatus, code, what) {
  status(res, httpStatus, what);
  const j = res.json;
  eq(j.error, code, `${what}: error code`);
  ok(typeof j.message === "string" && j.message.length > 0, `${what}: the Supabase "message" key is missing: ${res.text.slice(0, 200)}`);
  return j;
}

// ---- the node: sign-in, CLI, users -----------------------------------------------------------

function cli(...args) {
  const [cmd, ...pre] = cfg.cli;
  try {
    return execFileSync(cmd, [...pre, ...args], { encoding: "utf8", stdio: ["ignore", "pipe", "pipe"], timeout: 180000 });
  } catch (e) {
    throw new WalkError(`supavise ${args.join(" ")} failed: ${(e.stderr ?? "").toString().slice(0, 400)}${(e.stdout ?? "").toString().slice(0, 400)}`);
  }
}

async function signIn(email, password) {
  const r = await call("POST", "/auth/v1/token?grant_type=password", { json: { email, password } });
  status(r, 200, `sign-in of ${email}`);
  ok(typeof r.json.access_token === "string", "sign-in returned no access_token");
  return r.json.access_token;
}

// mkUser invites an address with a role (the claim page, as roles-smoke.sh does), claims it and signs in.
async function mkUser(name, role) {
  const email = `${name}@example.com`;
  const link = cli("users", "invite", email, "--role", role, "--org", org, "--no-mail").trim();
  ok(link.includes("/claim#"), `the invitation link of ${name} is not the claim page`);
  const token = new URLSearchParams(new URL(link).hash.slice(1)).get("token");
  ok(/^sbi_[0-9a-f]{48}$/.test(token ?? ""), `invitation token of ${name}`);
  const r = await call("POST", "/claim", { json: { token, email, password: cfg.password } });
  status(r, 201, `claim page for ${name}`);
  const jwt = await signIn(email, cfg.password);
  return { name, email, jwt, id: JSON.parse(Buffer.from(jwt.split(".")[1], "base64url").toString()).sub };
}

// ---- OAuth client side -----------------------------------------------------------------------

async function register(over = {}) {
  const body = {
    client_name: over.client_name ?? "oauth-walk client",
    redirect_uris: over.redirect_uris ?? [LOOPBACK],
    grant_types: ["authorization_code", "refresh_token"],
    response_types: ["code"],
    token_endpoint_auth_method: over.auth_method ?? "none",
    ...(over.scope ? { scope: over.scope } : {}),
  };
  const r = status(await call("POST", "/platform/oauth/apps/register", { json: body }), 201, "registration");
  const j = r.json;
  ok(UUID_RE.test(j.client_id), `client_id is not a UUID v4: ${j.client_id}`);
  eq(j.id, j.client_id, "registration: id equals client_id");
  ok(/^sba_[0-9a-f]{64}$/.test(j.client_secret), "registration: client_secret shape");
  eq(j.client_secret_expires_at, 0, "registration: client_secret_expires_at");
  eq(j.redirect_uris, body.redirect_uris, "registration: redirect_uris echo");
  return { client_id: j.client_id, secret: j.client_secret, redirect_uri: body.redirect_uris[0], auth_method: body.token_endpoint_auth_method, reg: j };
}

// authorize starts a request; over maps a parameter to its value, or to null to leave it out.
async function authorize(app, over = {}, pk = pkcePair()) {
  const state = crypto.randomBytes(12).toString("hex");
  const p = {
    client_id: app.client_id,
    response_type: "code",
    redirect_uri: app.redirect_uri,
    state,
    code_challenge: pk.challenge,
    code_challenge_method: "S256",
    resource: MCP_RESOURCE,
    ...over,
  };
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(p)) if (v !== null && v !== undefined) q.set(k, v);
  const res = await call("GET", `/v1/oauth/authorize?${q}`);
  return { res, state, pk, params: p };
}

// startAuth expects the redirect to Studio's consent page and returns the authorization id.
async function startAuth(app, over = {}) {
  const a = await authorize(app, over);
  status(a.res, 302, "authorize");
  const loc = new URL(a.res.h("location"));
  ok(`${loc.origin}${loc.pathname}` === `${dashboard}/authorize`, `authorize redirected to ${a.res.h("location")}, want ${dashboard}/authorize`);
  const authId = loc.searchParams.get("auth_id");
  ok(UUID_RE.test(authId ?? ""), `auth_id is not a UUID v4: ${authId}`);
  return { ...a, authId };
}

const approveURL = (slug, id) => `/platform/organizations/${slug}/oauth/authorizations/${id}`;

// approve returns the code the redirect carries, after checking the redirect's shape.
async function approve(jwt, slug, a) {
  const r = status(await call("POST", `${approveURL(slug, a.authId)}?skip_browser_redirect=true`, { token: jwt }), 201, "approve");
  eq(r.h("cache-control"), "no-store", "approve: Cache-Control");
  const u = new URL(r.json.url);
  const want = new URL(a.params.redirect_uri);
  ok(u.origin === want.origin && u.pathname === want.pathname, `approve: the redirect goes to ${u.origin}${u.pathname}`);
  const code = u.searchParams.get("code");
  ok(/^sbc_[0-9a-f]{64}$/.test(code ?? ""), "approve: code shape");
  eq(u.searchParams.get("state"), a.state, "approve: state echoed");
  eq(u.searchParams.get("iss"), issuer, "approve: iss");
  return code;
}

function clientAuth(app, how) {
  switch (how) {
    case "basic": return { headers: { authorization: basicHeader(app.client_id, app.secret) }, form: {} };
    case "post": return { headers: {}, form: { client_id: app.client_id, client_secret: app.secret } };
    default: return { headers: {}, form: { client_id: app.client_id } };
  }
}

function token(app, form, how = "none", extra = {}) {
  const ca = clientAuth(app, how);
  return call("POST", "/v1/oauth/token", { form: { ...ca.form, ...form }, headers: ca.headers, ...extra });
}

function redeem(app, code, pk, how = "none", over = {}) {
  return token(app, { grant_type: "authorization_code", code, redirect_uri: app.redirect_uri, code_verifier: pk.verifier, ...over }, how);
}

function checkTokenResponse(r, what) {
  status(r, 200, what);
  eq(r.h("cache-control"), "no-store", `${what}: Cache-Control`);
  eq(r.h("pragma"), "no-cache", `${what}: Pragma`);
  const j = r.json;
  ok(/^sbp_oauth_[0-9a-f]{40}$/.test(j.access_token), `${what}: access_token shape`);
  ok(/^sbr_[0-9a-f]{64}$/.test(j.refresh_token), `${what}: refresh_token shape`);
  eq(j.token_type, "Bearer", `${what}: token_type`);
  eq(j.expires_in, 3600, `${what}: expires_in`);
  ok(typeof j.scope === "string" && j.scope.length > 0, `${what}: scope`);
  return j;
}

// obtain runs a whole authorization: authorize, approve as user in slug, redeem. how is the client authentication.
async function obtain(app, user, slug, { how = "none", over = {} } = {}) {
  const a = await startAuth(app, over);
  const code = await approve(user.jwt, slug, a);
  return { ...checkTokenResponse(await redeem(app, code, a.pk, how), "token"), a, code };
}

const scopeSet = (s) => s.split(/\s+/).filter(Boolean).sort();

// ---- the walk --------------------------------------------------------------------------------

const owner = { name: "owner", email: cfg.owner.email, jwt: null };
let admin, dev;
let pub, conf; // a public (none) and a confidential (client_secret_post) dynamic app

await step("L-00", "the node answers and the owner signs in", async () => {
  owner.jwt = await signIn(cfg.owner.email, cfg.owner.password);
  const r = status(await call("GET", "/v1/organizations", { token: cfg.owner_pat }), 200, "owner PAT");
  ok(r.json.some((o) => o.slug === org) && r.json.some((o) => o.slug === org2), "the owner's PAT does not list both organizations");
});

await step("L-01", "/mcp without a token, and the two discovery documents", async () => {
  const r = status(await call("POST", "/mcp?project_ref=" + ref, { json: { jsonrpc: "2.0", id: 1, method: "tools/list" } }), 401, "POST /mcp without a token");
  eq(r.h("www-authenticate"), `Bearer resource_metadata="${PRM_URL}"`, "WWW-Authenticate of the gate");
  eq(r.json, { message: "No access token provided" }, "body of the gate's 401");

  const prm = status(await call("GET", "/.well-known/oauth-protected-resource/mcp"), 200, "protected resource metadata");
  eq(prm.h("cache-control"), "public, max-age=3600", "PRM Cache-Control");
  eq(prm.json, {
    resource: MCP_RESOURCE,
    authorization_servers: [issuer],
    bearer_methods_supported: ["header"],
    resource_name: "Supavise MCP",
    scopes_supported: SCOPES13,
  }, "protected resource metadata");

  const as = status(await call("GET", "/.well-known/oauth-authorization-server"), 200, "authorization server metadata");
  eq(as.h("cache-control"), "public, max-age=3600", "AS metadata Cache-Control");
  eq(as.json, {
    issuer,
    authorization_endpoint: `${issuer}/v1/oauth/authorize`,
    token_endpoint: `${issuer}/v1/oauth/token`,
    registration_endpoint: `${issuer}/platform/oauth/apps/register`,
    revocation_endpoint: `${issuer}/v1/oauth/revoke`,
    response_types_supported: ["code"],
    response_modes_supported: ["query"],
    grant_types_supported: ["authorization_code", "refresh_token"],
    token_endpoint_auth_methods_supported: ["client_secret_basic", "client_secret_post"],
    revocation_endpoint_auth_methods_supported: ["client_secret_basic", "client_secret_post"],
    code_challenge_methods_supported: ["S256"],
    scopes_supported: SCOPES13,
    authorization_response_iss_parameter_supported: true,
  }, "authorization server metadata");

  // Built from configuration, never from the Host header. The edge dispatches on Host, so a foreign one
  // never reaches the API through it; the loopback admin listener serves the API for any Host.
  if (cfg.admin_url) {
    const a = new URL(cfg.admin_url);
    const admin = { target: { host: a.hostname, port: Number(a.port) } };
    const evil = status(await call("GET", "/.well-known/oauth-authorization-server", { ...admin, host: "evil.example" }), 200, "metadata with a foreign Host on the admin listener");
    eq(evil.json.issuer, issuer, "issuer under Host: evil.example");
    eq(evil.json.token_endpoint, `${issuer}/v1/oauth/token`, "token endpoint under Host: evil.example");
    status(await call("GET", "/.well-known/oauth-authorization-server", { host: "evil.example" }), 404, "metadata with a foreign Host at the edge");
    // /mcp is not served on the loopback admin listener.
    const r2 = await call("POST", "/mcp", { ...admin, host: a.host, json: { jsonrpc: "2.0", id: 1, method: "tools/list" } });
    ok(r2.status >= 400 && !(r2.h("www-authenticate") ?? "").includes("resource_metadata"), `the admin listener serves /mcp: ${r2.describe()}`);
  } else {
    log("      no admin_url in the config: the foreign Host and the admin listener checks are skipped");
  }
  // The path form only: the root form is an unrouted path, which answers like any other (401 without a
  // credential, 404 with one).
  status(await call("GET", "/.well-known/oauth-protected-resource", { token: cfg.owner_pat }), 404, "root form of the protected resource metadata");
  status(await call("GET", "/.well-known/oauth-protected-resource"), [401, 404], "root form of the protected resource metadata without a credential");
});

await step("L-02", "dynamic registration: a loopback client, and what is refused", async () => {
  pub = await register({ client_name: "oauth-walk public", auth_method: "none" });
  conf = await register({ client_name: "oauth-walk confidential", auth_method: "client_secret_post", redirect_uris: ["https://client.example.com/cb", LOOPBACK] });
  conf.redirect_uri = "https://client.example.com/cb";
  const bad = async (body, code, what) => {
    const r = status(await call("POST", "/platform/oauth/apps/register", { json: { client_name: "walk", redirect_uris: [LOOPBACK], ...body } }), 400, what);
    eq(r.json.error, code, `${what}: error`);
    ok(typeof r.json.error_description === "string", `${what}: error_description`);
  };
  await bad({ redirect_uris: ["http://example.com/cb"] }, "invalid_redirect_uri", "http on a public host");
  await bad({ redirect_uris: ["javascript:alert(1)"] }, "invalid_redirect_uri", "javascript: URI");
  await bad({ redirect_uris: ["cursor://anysphere.cursor-mcp/oauth/callback"] }, "invalid_redirect_uri", "custom scheme");
  await bad({ redirect_uris: ["https://client.example.com/cb#frag"] }, "invalid_redirect_uri", "fragment");
  await bad({ redirect_uris: ["https://user:pw@client.example.com/cb"] }, "invalid_redirect_uri", "userinfo");
  await bad({ redirect_uris: Array.from({ length: 11 }, (_, i) => `https://c${i}.example.com/cb`) }, "invalid_redirect_uri", "eleven redirect URIs");
  await bad({ client_name: "" }, "invalid_client_metadata", "empty client_name");
  await bad({ grant_types: ["password"] }, "invalid_client_metadata", "grant type password");
  await bad({ response_types: ["token"] }, "invalid_client_metadata", "response type token");
  await bad({ scope: "nonsense:read" }, "invalid_client_metadata", "a scope that is not advertised");
  await bad({ token_endpoint_auth_method: "private_key_jwt" }, "invalid_client_metadata", "unknown client authentication method");
});

await step("L-03", "authorize: the redirect to Studio and every refusal", async () => {
  const a = await startAuth(pub);
  eq(a.res.h("cache-control"), "no-store", "authorize: Cache-Control");
  eq(a.res.h("referrer-policy"), "no-referrer", "authorize: Referrer-Policy");

  // No redirect before the client and the redirect URI are known good.
  const unknown = status((await authorize({ ...pub, client_id: crypto.randomUUID() })).res, 422, "unknown client");
  ok(!unknown.h("location"), "an unknown client was redirected");
  ok((unknown.h("content-type") ?? "").startsWith("text/html"), "the unknown-client page is not HTML");
  eq(unknown.h("content-security-policy"), "default-src 'none'; frame-ancestors 'none'", "error page CSP");
  eq(unknown.h("cache-control"), "no-store", "error page Cache-Control");
  const badRedirect = status((await authorize(pub, { redirect_uri: "https://evil.example/cb" })).res, 400, "redirect URI that is not registered");
  ok(!badRedirect.h("location"), "an unregistered redirect URI was redirected to");
  const xss = status((await authorize(pub, { redirect_uri: "<script>alert(1)</script>" })).res, 400, "markup in redirect_uri");
  ok(!xss.text.includes("<script>alert(1)"), "the error page echoes markup unescaped");
  // localhost is not 127.0.0.1; only the port may differ on a registered loopback URI.
  status((await authorize(pub, { redirect_uri: "http://localhost:53682/callback" })).res, 400, "localhost for a registered 127.0.0.1");
  status((await authorize(pub, { redirect_uri: "http://127.0.0.1:53682/other" })).res, 400, "another path on loopback");
  status((await authorize(pub, { redirect_uri: "http://127.0.0.1.evil.example:53682/callback" })).res, 400, "lookalike loopback host");
  status((await authorize(pub, { redirect_uri: "http://127.0.0.1:61234/callback" })).res, 302, "another port on loopback");

  // From here on errors go back to the client, with state and iss.
  const redirectError = async (over, code, what) => {
    const x = await authorize(pub, over);
    status(x.res, 302, what);
    const u = new URL(x.res.h("location"));
    eq(`${u.origin}${u.pathname}`, new URL(LOOPBACK).origin + new URL(LOOPBACK).pathname, `${what}: redirect target`);
    const got = u.searchParams.get("error");
    ok(Array.isArray(code) ? code.includes(got) : got === code, `${what}: error is ${got}, want ${code}`);
    eq(u.searchParams.get("state"), x.state, `${what}: state`);
    eq(u.searchParams.get("iss"), issuer, `${what}: iss`);
  };
  await redirectError({ code_challenge_method: "plain" }, "invalid_request", "PKCE plain");
  await redirectError({ code_challenge_method: "sha256" }, "invalid_request", "PKCE sha256");
  await redirectError({ code_challenge_method: null }, "invalid_request", "a challenge without a method");
  await redirectError({ code_challenge: null, code_challenge_method: null }, "invalid_request", "no PKCE for a dynamic app");
  await redirectError({ code_challenge: "short" }, "invalid_request", "a short challenge");
  await redirectError({ response_type: "token" }, "unsupported_response_type", "response_type token");
  await redirectError({ scope: "nonsense:read" }, "invalid_scope", "an unknown scope");
  await redirectError({ resource: "https://evil.example/mcp" }, "invalid_target", "a foreign resource");
  await redirectError({ response_mode: "fragment" }, ["invalid_request", "unsupported_response_type"], "response_mode fragment");
});

await step("L-04", "consent: describe, approve, decline and who may", async () => {
  admin = await mkUser("walkadmin", "administrator");
  dev = await mkUser("walkdev", "developer");

  const a = await startAuth(pub, { organization_slug: org });
  const d = status(await call("GET", `/platform/oauth/authorizations/${a.authId}`, { token: admin.jwt }), 200, "describe as Administrator").json;
  eq(d.name, "oauth-walk public", "describe: name");
  eq(d.domain, "127.0.0.1", "describe: domain");
  eq(d.redirect_uri, LOOPBACK, "describe: redirect_uri");
  eq(d.registration_type, "dynamic", "describe: registration_type");
  eq(scopeSet(d.scopes.join(" ")), [...SCOPES13].sort(), "describe: scopes");
  ok(!("icon" in d) || !d.icon, "describe: a dynamic app shows no icon");
  ok(!("approved_at" in d) || !d.approved_at, "describe: approved before the approval");
  status(await call("GET", `/platform/oauth/authorizations/${crypto.randomUUID()}`, { token: admin.jwt }), 404, "describe of an unknown id");
  status(await call("GET", `/platform/oauth/authorizations/${a.authId}`, { token: dev.jwt }), 403, "describe as Developer");
  status(await call("GET", `/platform/oauth/authorizations/${a.authId}`), 401, "describe without a session");
  status(await call("GET", `/platform/oauth/authorizations/${a.authId}`, { token: cfg.owner_pat }), 401, "describe with a PAT");

  // Who may approve: Owners and Administrators of the chosen organization.
  const post = (jwt, slug) => call("POST", `${approveURL(slug, a.authId)}?skip_browser_redirect=true`, { token: jwt });
  status(await post(dev.jwt, org), 403, "approve as Developer");
  status(await post(admin.jwt, org2), 403, "approve in an organization the Administrator is not a member of");
  status(await post(cfg.owner_pat, org), 401, "approve with a PAT");
  status(await call("POST", `${approveURL(org, a.authId)}?skip_browser_redirect=true`), 401, "approve without a session");
  // The hint the request carried: an Owner of both organizations still cannot approve in the other one.
  status(await post(owner.jwt, org2), 403, "approve outside the organization_slug hint");
  const code = await approve(admin.jwt, org, a);
  const d2 = status(await call("GET", `/platform/oauth/authorizations/${a.authId}`, { token: admin.jwt }), 200, "describe after approval").json;
  eq(d2.approved_organization_slug, org, "describe: approved_organization_slug");
  ok(typeof d2.approved_at === "string" && d2.approved_at.length > 0, "describe: approved_at after the approval");
  status(await post(admin.jwt, org), 409, "approve twice");
  status(await call("DELETE", approveURL(org, a.authId), { token: admin.jwt }), 409, "decline after approval");
  status(await redeem(pub, code, a.pk), 200, "redeem the approved code");

  // Decline.
  const b = await startAuth(pub);
  status(await call("DELETE", approveURL(org, b.authId), { token: dev.jwt }), 403, "decline as Developer");
  const dec = status(await call("DELETE", approveURL(org, b.authId), { token: admin.jwt }), 200, "decline");
  eq(dec.json, { id: b.authId }, "decline: body");
  status(await call("POST", `${approveURL(org, b.authId)}?skip_browser_redirect=true`, { token: admin.jwt }), 409, "approve after decline");
  status(await call("POST", `${approveURL(org, crypto.randomUUID())}?skip_browser_redirect=true`, { token: admin.jwt }), 404, "approve an unknown id");
});

await step("L-05", "token endpoint: client authentication, the verifier, replay", async () => {
  // A wrong verifier burns the code.
  const a = await startAuth(pub);
  const code = await approve(owner.jwt, org, a);
  oauthError(await redeem(pub, code, pkcePair(), "none"), 400, "invalid_grant", "wrong verifier");
  oauthError(await redeem(pub, code, a.pk), 400, "invalid_grant", "the code after a wrong verifier");

  // A code belongs to its client and its redirect URI.
  const b = await startAuth(conf);
  const codeB = await approve(owner.jwt, org, b);
  oauthError(await redeem(pub, codeB, b.pk), 400, "invalid_grant", "a code presented by another client");
  oauthError(await redeem(conf, codeB, b.pk, "post", { redirect_uri: "https://client.example.com/other" }), 400, "invalid_grant", "a different redirect_uri");

  // Client authentication.
  const c = await startAuth(conf);
  const codeC = await approve(owner.jwt, org, c);
  const wrong = await token({ ...conf, secret: "sba_" + "0".repeat(64) }, { grant_type: "authorization_code", code: codeC, redirect_uri: conf.redirect_uri, code_verifier: c.pk.verifier }, "basic");
  oauthError(wrong, 401, "invalid_client", "a wrong secret by Basic");
  ok(/^Basic/i.test(wrong.h("www-authenticate") ?? ""), "a Basic failure carries WWW-Authenticate: Basic");
  oauthError(await call("POST", "/v1/oauth/token", {
    form: { grant_type: "authorization_code", code: codeC, redirect_uri: conf.redirect_uri, code_verifier: c.pk.verifier, client_id: conf.client_id, client_secret: conf.secret },
    headers: { authorization: basicHeader(conf.client_id, "sba_" + "1".repeat(64)) },
  }), 400, "invalid_request", "conflicting credentials in the header and the body");
  oauthError(await call("POST", "/v1/oauth/token", { json: { grant_type: "authorization_code" } }), 400, "invalid_request", "a JSON body");
  oauthError(await token(conf, { grant_type: "client_credentials" }, "post"), 400, "unsupported_grant_type", "client_credentials");
  oauthError(await token(conf, { grant_type: "urn:ietf:params:oauth:grant-type:jwt-bearer", assertion: "x" }, "post"), 400, "unsupported_grant_type", "jwt-bearer");
  oauthError(await token({ client_id: crypto.randomUUID(), secret: conf.secret }, { grant_type: "refresh_token", refresh_token: "sbr_" + "0".repeat(64) }, "post"), 401, "invalid_client", "an unknown client");
  // The wrong-secret attempt did not burn the code; the right one redeems it.
  checkTokenResponse(await redeem(conf, codeC, c.pk, "post"), "confidential client, secret in the body");
  // A dynamic client may leave its secret out when its PKCE verifier is right, whatever method it registered
  // (design decision 10); a manual app may not (L-10).
  const c2 = await startAuth(conf);
  checkTokenResponse(await redeem(conf, await approve(owner.jwt, org, c2), c2.pk, "none"), "dynamic client without a secret, with PKCE");

  // A public dynamic client needs no secret, but a secret it presents must be right.
  const d = await startAuth(pub);
  const codeD = await approve(owner.jwt, org, d);
  oauthError(await token({ ...pub, secret: "sba_" + "2".repeat(64) }, { grant_type: "authorization_code", code: codeD, redirect_uri: pub.redirect_uri, code_verifier: d.pk.verifier }, "post"), 401, "invalid_client", "a public client with a wrong secret");
  checkTokenResponse(await redeem(pub, codeD, d.pk), "public client with PKCE");

  // Replay: the second redemption fails and kills what the first one issued.
  const e = await startAuth(pub);
  const codeE = await approve(owner.jwt, org, e);
  const first = checkTokenResponse(await redeem(pub, codeE, e.pk), "first redemption");
  status(await call("GET", "/v1/organizations", { token: first.access_token }), 200, "the token before the replay");
  oauthError(await redeem(pub, codeE, e.pk), 400, "invalid_grant", "replay of a redeemed code");
  status(await call("GET", "/v1/organizations", { token: first.access_token }), 401, "the token after a replay");
  oauthError(await token(pub, { grant_type: "refresh_token", refresh_token: first.refresh_token }, "post"), 400, "invalid_grant", "the refresh token after a replay");
});

let tok; // the owner's grant for the default organization, with all 13 scopes
await step("L-06", "an OAuth token: organization-bound, scope-checked, never on /platform", async () => {
  tok = await obtain(pub, owner, org);
  eq(scopeSet(tok.scope), [...SCOPES13].sort(), "granted scopes");
  const A = tok.access_token;
  const orgs = status(await call("GET", "/v1/organizations", { token: A }), 200, "list organizations").json;
  eq(orgs.map((o) => o.slug), [org], "an organization-bound token lists one organization");
  const projects = status(await call("GET", "/v1/projects", { token: A }), 200, "list projects").json;
  ok(projects.some((p) => p.id === ref || p.ref === ref), "the project of the organization is missing");
  ok(!projects.some((p) => p.id === ref2 || p.ref === ref2), "the other organization's project is listed");
  status(await call("GET", `/v1/projects/${ref}`, { token: A }), 200, "read the project");
  status(await call("POST", `/v1/projects/${ref}/database/query`, { token: A, json: { query: "select 1 as one" } }), 201, "SQL through database:write");
  status(await call("GET", `/v1/projects/${ref2}`, { token: A }), 403, "the other organization's project");
  status(await call("POST", `/v1/projects/${ref2}/database/query`, { token: A, json: { query: "select 1" } }), 403, "SQL on the other organization's project");
  // Not available to OAuth tokens, and nothing on /platform.
  const prof = status(await call("GET", "/v1/profile", { token: A }), 403, "/v1/profile");
  ok(/not available to OAuth tokens/.test(prof.text), `/v1/profile: unexpected message ${prof.text.slice(0, 120)}`);
  status(await call("POST", "/v1/organizations", { token: A, json: { name: "walk", tier: "tier_free" } }), 403, "creating an organization");
  status(await call("GET", "/platform/profile", { token: A }), 401, "/platform/profile with an OAuth token");
  status(await call("GET", `/platform/organizations/${org}/oauth/apps?type=authorized`, { token: A }), 401, "the Apps page API with an OAuth token");
  // An OAuth token cannot approve (JWT only), so it cannot mint another one.
  status(await call("POST", `${approveURL(org, crypto.randomUUID())}?skip_browser_redirect=true`, { token: A }), 401, "approve with an OAuth token");

  // Narrowed scopes: read-only SQL yes, writes no, and the challenge names the scope. Another app, because a
  // second approval of the same app by the same user in the same organization replaces the first grant.
  const narrow = await obtain(conf, owner, org, { how: "post", over: { scope: "organizations:read projects:read database:read" } });
  eq(scopeSet(narrow.scope), ["database:read", "organizations:read", "projects:read"], "the narrowed scopes");
  status(await call("POST", `/v1/projects/${ref}/database/query/read-only`, { token: narrow.access_token, json: { query: "select 1 as one" } }), 201, "read-only SQL with database:read");
  const w = status(await call("POST", `/v1/projects/${ref}/database/query`, { token: narrow.access_token, json: { query: "select 1" } }), 403, "SQL with database:read only");
  const ch = w.h("www-authenticate") ?? "";
  ok(ch.startsWith("Bearer ") && ch.includes('error="insufficient_scope"') && ch.includes('scope="database:write"') && ch.includes(`resource_metadata="${PRM_URL}"`), `insufficient_scope challenge: ${ch}`);
  status(await call("GET", `/v1/projects/${ref}/secrets`, { token: narrow.access_token }), 403, "secrets without secrets:read");
});

await step("L-06b", "the stdio MCP server and the CLI's calls with an OAuth token", async () => {
  if (!cfg.stdio) {
    log("L-06b skipped: no stdio MCP scripts configured");
    return;
  }
  // Every call these scripts make has to be inside the 13 scopes (the token holds exactly those).
  for (const [name, script, extra] of [["mcp-roles", cfg.stdio.roles, ["owner"]], ["mcp-smoke", cfg.stdio.smoke, []]]) {
    const args = [script, cfg.admin_url, ref, ...extra];
    const code = await new Promise((resolve) => {
      const child = spawn(process.execPath, args, { stdio: "inherit", env: { ...process.env, SUPABASE_ACCESS_TOKEN: tok.access_token } });
      child.on("exit", (c) => resolve(c));
    });
    ok(code === 0, `${name} with an OAuth access token exited ${code}`);
  }
});

await step("L-07", "refresh: rotation, reuse inside the grace window, narrowing", async () => {
  const x = await obtain(pub, owner, org);
  const r1 = checkTokenResponse(await token(pub, { grant_type: "refresh_token", refresh_token: x.refresh_token }, "post"), "refresh");
  ok(r1.refresh_token !== x.refresh_token && r1.access_token !== x.access_token, "a refresh returned the old tokens");
  status(await call("GET", "/v1/organizations", { token: r1.access_token }), 200, "the refreshed access token");
  // Two processes refreshing at once must not force a sign-in: the old token still works for a moment.
  const r2 = checkTokenResponse(await token(pub, { grant_type: "refresh_token", refresh_token: x.refresh_token }, "post"), "refresh reuse inside the grace window");
  status(await call("GET", "/v1/organizations", { token: r2.access_token }), 200, "the access token of the reused refresh");
  // A narrower scope is accepted; one outside the grant is not. Tokens carry no scopes of their own
  // (the grant does), so what a narrowed refresh changes is recorded here, not judged.
  const r3 = checkTokenResponse(await token(pub, { grant_type: "refresh_token", refresh_token: r2.refresh_token, scope: "organizations:read" }, "post"), "refresh with a narrower scope");
  ok(scopeSet(r3.scope).every((x) => SCOPES13.includes(x)), `the narrowed refresh answered scope ${r3.scope}`);
  log(`      refresh asking for organizations:read -> scope ${r3.scope.split(" ").length > 3 ? "unchanged (" + r3.scope.split(" ").length + " scopes)" : r3.scope}`);
  status(await call("GET", "/v1/organizations", { token: r3.access_token }), 200, "organizations:read after the narrowed refresh");
  oauthError(await token(pub, { grant_type: "refresh_token", refresh_token: r3.refresh_token, scope: "analytics:write" }, "post"), 400, "invalid_scope", "a scope outside the grant");
  // A dynamic app registered with token_endpoint_auth_method none has no secret to present: it redeems a
  // code with its PKCE verifier and refreshes with the token alone (the token rotates and a reuse revokes
  // the grant). An app that registered for a secret never gets either without it.
  const r4 = checkTokenResponse(await token(pub, { grant_type: "refresh_token", refresh_token: r3.refresh_token }, "none"), "refresh by a public client without a secret");
  ok(r4.refresh_token !== r3.refresh_token, "a refresh without a secret returned the old refresh token");
  const cx = await obtain(conf, owner, org, { how: "post" });
  oauthError(await token(conf, { grant_type: "refresh_token", refresh_token: cx.refresh_token }, "none"), 401, "invalid_client", "refresh by a confidential client without its secret");
  // Another client cannot use the refresh token.
  oauthError(await token(conf, { grant_type: "refresh_token", refresh_token: r4.refresh_token }, "post"), 400, "invalid_grant", "a refresh token of another client");
  // The resource, when sent, has to be the stored one.
  oauthError(await token(pub, { grant_type: "refresh_token", refresh_token: r4.refresh_token, resource: "https://evil.example/mcp" }, "post"), 400, "invalid_target", "a different resource on refresh");
});

await step("L-08", "revoke: client credentials, other clients' tokens, the 401 at the API and at /mcp", async () => {
  const x = await obtain(conf, owner, org, { how: "post" });
  const asJSON = (app, t) => call("POST", "/v1/oauth/revoke", { json: { client_id: app.client_id, client_secret: app.secret, refresh_token: t } });
  status(await asJSON(conf, "sbr_" + "0".repeat(64)), 204, "revoke of an unknown token");
  oauthError(await call("POST", "/v1/oauth/revoke", { json: { client_id: conf.client_id, client_secret: "sba_" + "4".repeat(64), refresh_token: x.refresh_token } }), 401, "invalid_client", "revoke with a wrong secret");
  status(await asJSON(pub, x.refresh_token), 204, "revoke of another client's token");
  status(await call("GET", "/v1/organizations", { token: x.access_token }), 200, "the token after another client's revoke");
  // The gate lets a valid token through (Studio, when it runs, answers; without Studio the forward fails).
  const gate = await call("POST", `/mcp?project_ref=${ref}`, { token: x.access_token, json: { jsonrpc: "2.0", id: 1, method: "tools/list" } });
  ok(gate.status !== 401 && gate.status !== 403 && gate.status !== 404, `/mcp refused a valid token: ${gate.describe()}`);
  if (!cfg.mcp) ok([502, 503, 504].includes(gate.status), `/mcp without Studio answered ${gate.status}, want a gateway error`);

  status(await asJSON(conf, x.refresh_token), 204, "revoke by the client");
  status(await call("GET", "/v1/organizations", { token: x.access_token }), 401, "the access token after revoke");
  oauthError(await token(conf, { grant_type: "refresh_token", refresh_token: x.refresh_token }, "post"), 400, "invalid_grant", "the refresh token after revoke");
  const invalid = status(await call("POST", `/mcp?project_ref=${ref}`, { token: x.access_token, json: { jsonrpc: "2.0", id: 1, method: "tools/list" } }), 401, "/mcp with a revoked token");
  const ch = invalid.h("www-authenticate") ?? "";
  ok(ch.startsWith("Bearer ") && ch.includes('error="invalid_token"') && ch.includes(`resource_metadata="${PRM_URL}"`), `invalid_token challenge: ${ch}`);

  // RFC 7009 form encoding with a Basic header.
  const y = await obtain(conf, owner, org, { how: "post" });
  status(await call("POST", "/v1/oauth/revoke", { form: { token: y.access_token, token_type_hint: "access_token" }, headers: { authorization: basicHeader(conf.client_id, conf.secret) } }), 204, "form revoke by access token");
  status(await call("GET", "/v1/organizations", { token: y.access_token }), 401, "the access token after a form revoke");

  // Tokens the gate must refuse: unknown, and a dashboard session.
  const unknownTok = "sbp_oauth_" + "5".repeat(40);
  const u = status(await call("POST", `/mcp?project_ref=${ref}`, { token: unknownTok, json: { jsonrpc: "2.0", id: 1, method: "tools/list" } }), 401, "/mcp with an unknown token");
  ok((u.h("www-authenticate") ?? "").includes('error="invalid_token"'), "unknown token: invalid_token");
  const s = status(await call("POST", `/mcp?project_ref=${ref}`, { token: owner.jwt, json: { jsonrpc: "2.0", id: 1, method: "tools/list" } }), 401, "/mcp with a dashboard session");
  ok((s.h("www-authenticate") ?? "").includes('error="invalid_token"'), "dashboard session: invalid_token");
  // A grant bound to the issuer itself, not to /mcp, is refused at /mcp.
  const bound = await obtain(pub, owner, org, { over: { resource: issuer } });
  status(await call("GET", "/v1/organizations", { token: bound.access_token }), 200, "a grant bound to the issuer, at the API");
  const rb = status(await call("POST", `/mcp?project_ref=${ref}`, { token: bound.access_token, json: { jsonrpc: "2.0", id: 1, method: "tools/list" } }), 401, "/mcp with a grant bound to another resource");
  ok((rb.h("www-authenticate") ?? "").includes('error="invalid_token"'), "resource binding: invalid_token");
});

if (cfg.mcp) {
  await step("L-09", "MCP over Streamable HTTP at /mcp (Studio's route)", async () => {
    const t = (await obtain(pub, owner, org)).access_token;
    let nextId = 100;
    async function rpc(bearer, query, method, params, version = "2025-06-18") {
      const id = nextId++;
      const r = await call("POST", `/mcp${query}`, {
        token: bearer,
        headers: { accept: "application/json, text/event-stream", "mcp-protocol-version": version },
        json: { jsonrpc: "2.0", id, method, params },
      });
      status(r, 200, `${method} ${query}`);
      const type = r.h("content-type") ?? "";
      const msgs = type.includes("text/event-stream") ? sseMessages(r.text) : [r.json];
      const m = msgs.find((x) => x.id === id);
      ok(m, `${method}: no JSON-RPC answer with id ${id}: ${r.text.slice(0, 200)}`);
      return m;
    }
    const names = (m) => (m.result?.tools ?? []).map((x) => x.name);
    const text = (m) => (m.result?.content ?? []).map((c) => c.text ?? "").join("\n");
    const proj = `?project_ref=${ref}`;

    // Two protocol revisions: the answer is the server's own revision when it does not know the asked one.
    for (const v of ["2025-06-18", "2025-11-25"]) {
      const m = await rpc(t, proj, "initialize", { protocolVersion: v, capabilities: {}, clientInfo: { name: "oauth-walk", version: "0" } }, v);
      ok(!m.error && typeof m.result?.protocolVersion === "string", `initialize with ${v}: ${JSON.stringify(m).slice(0, 200)}`);
      log(`      initialize ${v} -> server speaks ${m.result.protocolVersion}`);
    }
    const tools = names(await rpc(t, proj, "tools/list", {}));
    for (const n of ["execute_sql", "list_tables", "apply_migration", "get_project_url"]) ok(tools.includes(n), `tools/list lacks ${n}: ${tools.join(",")}`);
    for (const n of ["list_organizations", "create_project", "list_projects"]) ok(!tools.includes(n), `a project-scoped URL offers the account tool ${n}`);

    const sql = await rpc(t, proj, "tools/call", { name: "execute_sql", arguments: { query: "select 1 as one" } });
    ok(!sql.error && !sql.result?.isError && /one/.test(text(sql)), `execute_sql: ${JSON.stringify(sql).slice(0, 300)}`);
    const url = await rpc(t, proj, "tools/call", { name: "get_project_url", arguments: {} });
    ok(text(url).includes(`${ref}.api.${domain}`), `get_project_url does not name ${ref}.api.${domain}: ${text(url).slice(0, 200)}`);

    // read_only=true: no write tools, and SQL that writes is refused.
    const ro = `${proj}&read_only=true`;
    const roTools = names(await rpc(t, ro, "tools/list", {}));
    ok(roTools.includes("execute_sql") && !roTools.includes("apply_migration"), `read_only tools: ${roTools.join(",")}`);
    const wr = await rpc(t, ro, "tools/call", { name: "execute_sql", arguments: { query: "create table public.walk_ro_must_fail (id int)" } });
    ok(wr.error || wr.result?.isError, `a write went through read_only=true: ${JSON.stringify(wr).slice(0, 300)}`);
    // Anything but true or false fails closed.
    const badRO = await call("POST", `/mcp${proj}&read_only=maybe`, { token: t, headers: { accept: "application/json, text/event-stream" }, json: { jsonrpc: "2.0", id: 1, method: "tools/list" } });
    eq(badRO.status, 400, "read_only=maybe");
    const badRef = await call("POST", `/mcp?project_ref=not-a-ref`, { token: t, headers: { accept: "application/json, text/event-stream" }, json: { jsonrpc: "2.0", id: 1, method: "tools/list" } });
    eq(badRef.status, 400, "a malformed project_ref");

    // features=docs narrows the tool set.
    const docs = names(await rpc(t, `${proj}&features=docs`, "tools/list", {}));
    ok(docs.includes("search_docs") && !docs.includes("execute_sql"), `features=docs tools: ${docs.join(",")}`);

    // A PAT works as a bearer, as on hosted.
    const patTools = names(await rpc(cfg.owner_pat, proj, "tools/list", {}));
    ok(patTools.includes("execute_sql"), "a personal access token is refused at /mcp");

    // Account-scoped URL: the account tools exist and the token's organization binding shows.
    const acc = await rpc(t, "", "tools/list", {});
    for (const n of ["list_organizations", "list_projects", "create_project"]) ok(names(acc).includes(n), `an account-scoped URL lacks ${n}`);
    const orgsTxt = text(await rpc(t, "", "tools/call", { name: "list_organizations", arguments: {} }));
    ok(orgsTxt.includes(org) && !orgsTxt.includes(org2), `list_organizations is not bound to ${org}: ${orgsTxt.slice(0, 300)}`);
    const projTxt = text(await rpc(t, "", "tools/call", { name: "list_projects", arguments: {} }));
    ok(projTxt.includes(ref) && !projTxt.includes(ref2), "list_projects is not bound to the token's organization");

    // How create_project behaves on this route (risk R12: stateless JSON cannot ask the client to confirm
    // a cost). Recorded, not judged: the exchange has to be well-formed and must not be a gateway error.
    const cost = await rpc(t, "", "tools/call", { name: "get_cost", arguments: { type: "project", organization_id: org } });
    log(`      get_cost -> ${cost.error ? "error " + JSON.stringify(cost.error).slice(0, 120) : text(cost).replace(/\s+/g, " ").slice(0, 120)}`);
    const cp = await rpc(t, "", "tools/call", { name: "create_project", arguments: { name: "walk-must-not-exist", organization_id: org, region: "us-east-1", confirm_cost_id: "none" } });
    log(`      create_project without a confirmed cost -> ${cp.error ? "error " + JSON.stringify(cp.error).slice(0, 120) : (cp.result?.isError ? "tool error: " : "result: ") + text(cp).replace(/\s+/g, " ").slice(0, 160)}`);
    ok(cp.error || cp.result?.isError, "create_project went through without a confirmed cost");

    // Studio's own route is not reachable on the dashboard host.
    const direct = await call("POST", "/api/mcp", { host: new URL(dashboard).host, token: t, json: { jsonrpc: "2.0", id: 1, method: "tools/list" } });
    eq(direct.status, 404, "studio.<domain>/api/mcp");

    // The pages that approve access cannot be framed, whatever Studio sends (the proxy sets the headers).
    for (const p of [`/authorize?auth_id=${crypto.randomUUID()}`, "/cli/login?session_id=walk"]) {
      const f = await call("GET", p, { host: new URL(dashboard).host, headers: { accept: "text/html" } });
      const name = p.split("?")[0];
      eq((f.h("x-frame-options") ?? "").toUpperCase(), "DENY", `${name}: X-Frame-Options`);
      const csp = f.h("content-security-policy") ?? "";
      ok(/(^|[;,]\s*)frame-ancestors\s+'none'\s*([;,]|$)/i.test(csp), `${name}: Content-Security-Policy lacks frame-ancestors 'none': ${csp.slice(0, 200)}`);
    }
  });
}

await step("L-10", "the organization's OAuth Apps API: authorized list, revoke, manual apps and secrets", async () => {
  const apps = `/platform/organizations/${org}/oauth/apps`;
  status(await call("GET", `${apps}?type=authorized`, { token: dev.jwt }), 403, "Apps API as Developer");
  status(await call("GET", `${apps}?type=authorized`), 401, "Apps API without a session");

  // Authorized: one item per app with a live grant in this organization. Two users, so that neither
  // approval replaces the other's grant.
  const g = await obtain(pub, admin, org);
  const g2 = await obtain(pub, owner, org);
  const list = status(await call("GET", `${apps}?type=authorized`, { token: admin.jwt }), 200, "authorized apps").json;
  const mine = list.filter((x) => x.client_id === pub.client_id);
  eq(mine.length, 1, "the authorized list shows the client once");
  eq(mine[0].id, pub.client_id, "authorized: id equals client_id");
  eq(mine[0].app_id, pub.client_id, "authorized: app_id equals client_id");
  eq(mine[0].name, "oauth-walk public", "authorized: name");
  ok(Array.isArray(mine[0].scopes) && mine[0].scopes.length > 0, "authorized: scopes");
  ok(!("icon" in mine[0]) || !mine[0].icon, "authorized: a dynamic app has no icon");
  // Revoke for every user of the organization.
  const rv = status(await call("POST", `${apps}/${pub.client_id}/revoke`, { token: admin.jwt }), 201, "revoke the app").json;
  eq(rv.id, pub.client_id, "revoke: id");
  status(await call("GET", "/v1/organizations", { token: g.access_token }), 401, "the token after the app was revoked");
  status(await call("GET", "/v1/organizations", { token: g2.access_token }), 401, "every grant of the app in the organization is gone");
  const after = status(await call("GET", `${apps}?type=authorized`, { token: admin.jwt }), 200, "authorized apps after the revoke").json;
  ok(!after.some((x) => x.client_id === pub.client_id), "a revoked app is still listed as authorized");

  // Manual app.
  eq(status(await call("GET", `${apps}?type=published`, { token: admin.jwt }), 200, "published apps").json.filter((x) => x.name === "oauth-walk manual").length, 0, "no manual app yet");
  const cb = "https://client.example.com/cb";
  const created = status(await call("POST", apps, { token: admin.jwt, json: { name: "oauth-walk manual", website: "https://example.com", scopes: ["organizations:read", "projects:read", "database:read"], redirect_uris: [cb] } }), 201, "create a manual app").json;
  ok(UUID_RE.test(created.id) && created.id === created.client_id, "manual app: id is a UUID and equals client_id");
  ok(/^sba_[0-9a-f]{64}$/.test(created.client_secret), "manual app: the first client secret");
  eq(created.client_secret_expires_at, 0, "manual app: client_secret_expires_at");
  eq(created.redirect_uris, [cb], "manual app: redirect_uris");
  status(await call("POST", apps, { token: admin.jwt, json: { name: "x", website: "https://example.com", scopes: ["nonsense:read"], redirect_uris: [cb] } }), [400, 422], "a scope outside the 24");
  status(await call("POST", apps, { token: admin.jwt, json: { name: "x", website: "https://example.com", scopes: ["organizations:read"], redirect_uris: ["http://example.com/cb"] } }), [400, 422], "http redirect URI on a manual app");
  const pubd = status(await call("GET", `${apps}?type=published`, { token: admin.jwt }), 200, "published apps").json;
  ok(pubd.some((x) => x.client_id === created.client_id), "the manual app is not in the published list");

  const secrets = `${apps}/${created.client_id}/client-secrets`;
  const sl = status(await call("GET", secrets, { token: admin.jwt }), 200, "list client secrets");
  ok(!sl.text.includes(created.client_secret), "the client-secret list shows a plaintext secret");
  eq(sl.json.client_secrets.length, 1, "one client secret");
  ok(/^sba_[0-9a-f]{4}\*+$/.test(sl.json.client_secrets[0].client_secret_alias), `secret alias: ${sl.json.client_secrets[0].client_secret_alias}`);
  const first = sl.json.client_secrets[0].id;
  ok(UUID_RE.test(first), "client secret id is a UUID");
  const sec2 = status(await call("POST", secrets, { token: admin.jwt }), 201, "create a second client secret").json;
  ok(/^sba_[0-9a-f]{64}$/.test(sec2.client_secret) && UUID_RE.test(sec2.id), "second secret: shape");
  eq(sec2.oauth_app_id, created.client_id, "second secret: oauth_app_id");
  status(await call("GET", `${apps}/${pub.client_id}/client-secrets`, { token: admin.jwt }), 404, "client secrets of a dynamic app");

  // A full code flow with the manual app's secret in a Basic header (PKCE is optional for a manual app; we send it).
  const man = { client_id: created.client_id, secret: created.client_secret, redirect_uri: cb };
  const a = await startAuth(man, { scope: "organizations:read projects:read" });
  const code = await approve(admin.jwt, org, a);
  oauthError(await token(man, { grant_type: "authorization_code", code, redirect_uri: cb, code_verifier: a.pk.verifier }, "none"), 401, "invalid_client", "a manual app without its secret");
  const tr = checkTokenResponse(await redeem(man, code, a.pk, "basic"), "manual app with client_secret_basic");
  eq(scopeSet(tr.scope), ["organizations:read", "projects:read"], "manual app: scopes");
  status(await call("GET", "/v1/organizations", { token: tr.access_token }), 200, "manual app token");
  // The secret list shows the use; the first secret can go, and it stops working.
  const second = { ...man, secret: sec2.client_secret };
  status(await call("DELETE", `${secrets}/${first}`, { token: admin.jwt }), 200, "delete the first secret");
  const b = await startAuth(second, { scope: "organizations:read" });
  const codeB = await approve(admin.jwt, org, b);
  oauthError(await redeem(man, codeB, b.pk, "basic"), 401, "invalid_client", "a deleted client secret");
  const tr2 = checkTokenResponse(await redeem(second, codeB, b.pk, "basic"), "the second secret");
  // The same app, user and organization approving again replaces the older grant.
  status(await call("GET", "/v1/organizations", { token: tr.access_token }), 401, "the older grant after a second approval of the same app, user and organization");
  // Narrowing the app narrows live grants at once.
  status(await call("GET", `/v1/projects/${ref}`, { token: tr2.access_token }), 403, "projects:read was not in the second grant");
  const put = await call("PUT", `${apps}/${created.client_id}`, { token: admin.jwt, json: { name: "oauth-walk manual", website: "https://example.com", scopes: ["projects:read"], redirect_uris: [cb] } });
  status(put, 200, "update the manual app");
  status(await call("GET", "/v1/organizations", { token: tr2.access_token }), 403, "organizations:read after the app dropped it");
  // Delete: the grants die and the client is unknown.
  status(await call("DELETE", `${apps}/${created.client_id}`, { token: dev.jwt }), 403, "delete as Developer");
  status(await call("DELETE", `${apps}/${created.client_id}`, { token: admin.jwt }), 200, "delete the manual app");
  status(await call("GET", "/v1/organizations", { token: tr2.access_token }), 401, "the token of a deleted app");
  status((await authorize(man)).res, 422, "authorize for a deleted app");
});

await step("L-11", "removing a user and the operator CLI end grants at once", async () => {
  // users remove
  const gone = await mkUser("walkgone", "administrator");
  const g = await obtain(pub, gone, org);
  status(await call("GET", "/v1/organizations", { token: g.access_token }), 200, "the grant of a user before the removal");
  cli("users", "remove", gone.email);
  status(await call("GET", "/v1/organizations", { token: g.access_token }), 401, "the grant of a removed user");

  // oauth grants list and revoke by user, then by app.
  const h = await obtain(pub, admin, org);
  const listing = cli("oauth", "grants", "list", "--user", admin.email);
  ok(listing.includes("oauth-walk public") || listing.includes(pub.client_id), `oauth grants list --user does not show the client:\n${listing.slice(0, 400)}`);
  cli("oauth", "grants", "revoke", "--user", admin.email, "--yes");
  status(await call("GET", "/v1/organizations", { token: h.access_token }), 401, "the grant after oauth grants revoke --user");
  const i = await obtain(pub, owner, org);
  status(await call("GET", "/v1/organizations", { token: i.access_token }), 200, "a new grant");
  cli("oauth", "grants", "revoke", "--app", pub.client_id, "--yes");
  status(await call("GET", "/v1/organizations", { token: i.access_token }), 401, "the grant after oauth grants revoke --app");
});

await step("L-12", "CORS: the open policy on the OAuth paths only", async () => {
  const pre = async (path, method = "POST", headers = "authorization, content-type") => {
    const r = await call("OPTIONS", path, { headers: { origin: "https://claude.ai", "access-control-request-method": method, "access-control-request-headers": headers } });
    status(r, 204, `preflight ${path}`);
    eq(r.h("access-control-allow-origin"), "*", `${path}: Allow-Origin`);
    ok(r.h("access-control-allow-credentials") === undefined, `${path}: the open policy allows credentials`);
    const allowed = (r.h("access-control-allow-headers") ?? "").toLowerCase();
    ok(allowed.includes("authorization") && allowed.includes("content-type"), `${path}: Allow-Headers is ${allowed}`);
    ok((r.h("access-control-allow-methods") ?? "").includes("POST"), `${path}: Allow-Methods`);
    return r;
  };
  await pre("/v1/oauth/token");
  await pre("/v1/oauth/revoke");
  await pre("/platform/oauth/apps/register");
  await pre("/.well-known/oauth-authorization-server", "GET");
  await pre("/.well-known/oauth-protected-resource/mcp", "GET");
  const m = await pre("/mcp", "POST", "authorization, content-type, mcp-protocol-version, mcp-session-id");
  ok((m.h("access-control-allow-headers") ?? "").toLowerCase().includes("mcp-protocol-version"), "/mcp: Allow-Headers lacks mcp-protocol-version");
  // Answers carry the policy too, and /mcp lets a browser client read the challenge.
  const g = status(await call("GET", "/.well-known/oauth-authorization-server", { headers: { origin: "https://claude.ai" } }), 200, "metadata with an Origin");
  eq(g.h("access-control-allow-origin"), "*", "metadata: Allow-Origin");
  const u = status(await call("POST", "/mcp", { headers: { origin: "https://claude.ai" }, json: { jsonrpc: "2.0", id: 1, method: "tools/list" } }), 401, "/mcp with an Origin");
  eq(u.h("access-control-allow-origin"), "*", "/mcp 401: Allow-Origin");
  ok((u.h("access-control-expose-headers") ?? "").toLowerCase().includes("www-authenticate"), "/mcp 401: Expose-Headers lacks WWW-Authenticate");
  // Everything else keeps the dashboard policy: no wildcard, no echo of a foreign origin.
  const other = await call("OPTIONS", "/v1/projects", { headers: { origin: "https://claude.ai", "access-control-request-method": "GET", "access-control-request-headers": "authorization" } });
  const ao = other.h("access-control-allow-origin");
  ok(ao !== "*" && ao !== "https://claude.ai", `a path outside the OAuth set allows ${ao}`);
  const other2 = await call("GET", "/v1/oauth/authorize?client_id=x", { headers: { origin: "https://claude.ai" } });
  ok(other2.h("access-control-allow-origin") !== "*", "the authorize endpoint got the open policy");
});

await step("L-13", "roles are live and membership ends the grant", async () => {
  const w = await obtain(pub, admin, org);
  const query = (q) => call("POST", `/v1/projects/${ref}/database/query`, { token: w.access_token, json: { query: q } });
  status(await query("create table if not exists public.walk_t (id int primary key, v text)"), 201, "write as Administrator");
  status(await query("insert into public.walk_t values (1, 'a') on conflict do nothing"), 201, "insert as Administrator");
  // Demote to Read-only (role 4): the same token loses its writes, keeps its reads.
  const members = `/platform/organizations/${org}/members`;
  status(await call("PATCH", `${members}/${admin.id}`, { token: owner.jwt, json: { role_id: 4 } }), 200, "demote the Administrator to Read-only");
  const denied = await query("insert into public.walk_t values (2, 'b')");
  ok(denied.status >= 400 && denied.status < 500, `a demoted member's token still writes: ${denied.describe()}`);
  status(await query("select v from public.walk_t"), 201, "read as Read-only");
  status(await call("POST", `/v1/projects/${ref}/database/migrations`, { token: w.access_token, json: { query: "create table public.walk_m (id int)", name: "walk_m" } }), 403, "a migration as Read-only");
  // Remove from the organization: 401 from then on.
  status(await call("DELETE", `${members}/${admin.id}`, { token: owner.jwt }), 200, "remove the member");
  status(await call("GET", "/v1/organizations", { token: w.access_token }), 401, "the token of a removed member");
});

await step("L-14", "the limiters: 100 registrations and 30 token failures a minute, and the node stays up", async () => {
  // Last, because both use up the allowance of this address. The registrations of the walk count too.
  let limited = null;
  let sent = 0;
  for (; sent < 140 && !limited; sent++) {
    const r = await call("POST", "/platform/oauth/apps/register", { json: { client_name: `walk flood ${sent}`, redirect_uris: [LOOPBACK], token_endpoint_auth_method: "none" } });
    if (r.status === 429) limited = r;
    else status(r, 201, `registration ${sent}`);
  }
  ok(limited, "140 registrations from one address were not limited");
  ok(Number(limited.h("retry-after")) > 0, "the registration limiter's 429 has no Retry-After");
  log(`      the registration limiter answered 429 after ${sent - 1} more registrations`);

  limited = null;
  for (let i = 0; i < 60 && !limited; i++) {
    const r = await token({ client_id: crypto.randomUUID(), secret: "sba_" + "3".repeat(64) }, { grant_type: "refresh_token", refresh_token: "sbr_" + "0".repeat(64) }, "post");
    if (r.status === 429) limited = r;
  }
  ok(limited, "60 failed token requests from one address were not limited");
  ok(Number(limited.h("retry-after")) > 0, "the token limiter's 429 has no Retry-After");

  // The node still serves.
  status(await call("GET", "/v1/organizations", { token: cfg.owner_pat }), 200, "the API after the floods");
  status(await call("GET", "/.well-known/oauth-authorization-server"), 200, "metadata after the floods");
});

log("oauth walk passed");
