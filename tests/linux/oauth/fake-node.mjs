// A stand-in for a Supavise node that answers the way docs/design.md section 13 says it does, so that
// oauth-walk.mjs can be checked on a development machine (see selftest.mjs). It is not the server:
// it keeps everything in memory, skips MFA and SSO, and implements only the routes the walk calls.
// When the walk and the real server disagree, the design decides which one is wrong.
import crypto from "node:crypto";
import http from "node:http";

const ADVERTISED = [
  "organizations:read", "projects:read", "projects:write", "database:read", "database:write",
  "analytics:read", "secrets:read", "edge_functions:read", "edge_functions:write",
  "environment:read", "environment:write", "storage:read", "storage:write",
];
const ALL24 = [...ADVERTISED, "analytics:write", "analytics_config:read", "analytics_config:write", "auth:read", "auth:write",
  "domains:read", "domains:write", "organizations:write", "rest:read", "rest:write", "secrets:write"];
const ROLE_IDS = { 1: "owner", 2: "administrator", 3: "developer", 4: "read-only" };
const hex = (n) => crypto.randomBytes(n).toString("hex");
const sha = (s) => crypto.createHash("sha256").update(s).digest("hex");
const uuid = () => crypto.randomUUID();

export async function startFake({ mcp = false, domain = "supavise.test" } = {}) {
  const issuer = `http://api.${domain}`;
  const dashboard = `http://studio.${domain}`;
  const now = () => Date.now();
  const orgs = [{ id: 1, slug: "default", name: "Default" }, { id: 2, slug: "second", name: "Second" }];
  const projects = [{ ref: "a".repeat(20), org: 1 }, { ref: "b".repeat(20), org: 2 }];
  const users = new Map(); // email -> {id, email, password, roles: {orgId: role}, removed}
  const jwts = new Map(); // jwt -> userId
  const invites = new Map();
  const apps = new Map(); // id -> app
  const auths = new Map(); // id -> authorization
  const grants = []; const tokens = new Map(); // value -> token
  const pat = "sbp_" + hex(20);
  const addUser = (email, password, roles) => { const u = { id: uuid(), email, password, roles, removed: false }; users.set(email, u); return u; };
  const owner = addUser("owner@example.com", "owner-pw", { 1: "owner", 2: "owner" });
  const fails = { token: [], register: [], authorize: [] };
  const hit = (list, windowMs, max) => { const t = now(); while (list.length && t - list[0] > windowMs) list.shift(); list.push(t); return list.length > max; };

  const jwtFor = (u) => { const j = `eyJhbGciOiJIUzI1NiJ9.${Buffer.from(JSON.stringify({ sub: u.id, email: u.email, n: hex(4) })).toString("base64url")}.${hex(8)}`; jwts.set(j, u.id); return j; };
  const userById = (id) => [...users.values()].find((u) => u.id === id);

  const server = http.createServer(async (req, res) => {
    const chunks = [];
    for await (const c of req) chunks.push(c);
    const raw = Buffer.concat(chunks).toString("utf8");
    const url = new URL(req.url, "http://x");
    const host = (req.headers.host ?? "").split(":")[0];
    const ctype = req.headers["content-type"] ?? "";
    const q = url.searchParams;
    const out = (status, body, headers = {}) => {
      const h = { ...headers };
      if (typeof body === "object" && body !== null) { h["content-type"] ??= "application/json"; body = JSON.stringify(body); }
      if (corsOpen(url.pathname)) Object.assign(h, { "access-control-allow-origin": "*", ...(url.pathname === "/mcp" ? { "access-control-expose-headers": "WWW-Authenticate, Mcp-Session-Id" } : {}) });
      res.writeHead(status, h);
      res.end(body ?? "");
    };
    const err = (status, message, headers) => out(status, { message }, headers);
    const oerr = (status, error, message, headers) => out(status, { error, message, error_description: message }, { "cache-control": "no-store", ...headers });
    const body = () => { if (!raw) return {}; try { return JSON.parse(raw); } catch { return null; } };
    const clientIP = "127.0.0.1";
    void clientIP;

    // ---- internal hooks for fake-cli.mjs
    if (url.pathname.startsWith("/__cli/")) return cliRoute(url, q, out, err);

    // ---- the dashboard host
    if (host === `studio.${domain}`) {
      if (url.pathname === "/api/mcp") return err(404, "Not Found");
      return err(404, "no studio here");
    }

    // ---- CORS
    if (req.method === "OPTIONS" && corsOpen(url.pathname)) {
      const base = { "access-control-allow-origin": "*", "access-control-allow-methods": "GET, POST, DELETE, OPTIONS", "access-control-allow-headers": "authorization, content-type, mcp-protocol-version, mcp-session-id, last-event-id" };
      res.writeHead(204, base); return res.end();
    }
    if (req.method === "OPTIONS") { res.writeHead(204, { "access-control-allow-origin": "https://studio-only.example" }); return res.end(); }

    const R = (m, p) => { if (req.method !== m) return null; const a = p.split("/"), b = url.pathname.split("/"); if (a.length !== b.length) return null; const params = {}; for (let i = 0; i < a.length; i++) { if (a[i].startsWith(":")) params[a[i].slice(1)] = decodeURIComponent(b[i]); else if (a[i] !== b[i]) return null; } return params; };
    let m;

    // ---- discovery
    if (R("GET", "/.well-known/oauth-protected-resource/mcp")) return out(200, { resource: `${issuer}/mcp`, authorization_servers: [issuer], bearer_methods_supported: ["header"], resource_name: "Supavise MCP", scopes_supported: ADVERTISED }, { "cache-control": "public, max-age=3600" });
    if (R("GET", "/.well-known/oauth-authorization-server")) return out(200, {
      issuer, authorization_endpoint: `${issuer}/v1/oauth/authorize`, token_endpoint: `${issuer}/v1/oauth/token`, registration_endpoint: `${issuer}/platform/oauth/apps/register`,
      revocation_endpoint: `${issuer}/v1/oauth/revoke`, response_types_supported: ["code"], response_modes_supported: ["query"], grant_types_supported: ["authorization_code", "refresh_token"],
      token_endpoint_auth_methods_supported: ["client_secret_basic", "client_secret_post"], revocation_endpoint_auth_methods_supported: ["client_secret_basic", "client_secret_post"],
      code_challenge_methods_supported: ["S256"], scopes_supported: ADVERTISED, authorization_response_iss_parameter_supported: true,
    }, { "cache-control": "public, max-age=3600" });
    if (url.pathname.startsWith("/.well-known/")) return err(404, "Not Found");

    // ---- dashboard sign-in and the claim page
    if (R("POST", "/auth/v1/token")) { const b = body(); const u = users.get(b?.email); if (!u || u.removed || u.password !== b.password) return err(400, "Invalid login credentials"); return out(200, { access_token: jwtFor(u), token_type: "bearer" }); }
    if (R("POST", "/claim")) { const b = body(); const inv = invites.get(b?.token); if (!inv || inv.email !== b.email) return err(403, "Invalid token"); invites.delete(b.token); addUser(b.email, b.password, { [inv.orgId]: inv.role }); return out(201, { email: b.email }); }

    // ---- who is calling
    const bearer = (req.headers.authorization ?? "").match(/^Bearer (.+)$/i)?.[1];
    const principal = () => {
      if (!bearer) return null;
      if (bearer === pat) return { via: "pat", user: owner, orgs: [1, 2] };
      if (bearer.startsWith("sbp_oauth_")) {
        const t = tokens.get(bearer);
        if (!t || t.kind !== "access" || t.expires < now()) return null;
        const g = grants[t.grant]; const app = apps.get(g.app);
        if (g.revoked || app.deleted) return null;
        const u = userById(g.user);
        if (!u || u.removed) return null;
        if (!u.roles[g.org]) { g.revoked = "membership"; return null; }
        const scopes = g.scopes.filter((s) => app.scopes.includes(s));
        return { via: "oauth", user: u, orgs: [g.org], scopes, grant: g };
      }
      const uid = jwts.get(bearer); const u = uid && userById(uid);
      return u && !u.removed ? { via: "jwt", user: u, orgs: Object.keys(u.roles).map(Number) } : null;
    };
    const roleIn = (p, orgId) => (p.orgs.includes(orgId) ? p.user.roles[orgId] : undefined);
    const orgBySlug = (s) => orgs.find((o) => o.slug === s);

    // ---- /mcp: the edge gate
    if (url.pathname === "/mcp") {
      const prm = `Bearer resource_metadata="${issuer}/.well-known/oauth-protected-resource/mcp"`;
      if (!bearer) return out(401, { message: "No access token provided" }, { "www-authenticate": prm });
      const p = principal();
      const bad = (d) => out(401, { message: d }, { "www-authenticate": `Bearer error="invalid_token", error_description="${d}", resource_metadata="${issuer}/.well-known/oauth-protected-resource/mcp"` });
      if (!p || p.via === "jwt") return bad("The access token is invalid");
      if (p.grant && p.grant.resource && p.grant.resource !== `${issuer}/mcp`) return bad("The token is for another resource");
      if (!mcp) return err(502, "Bad Gateway");
      return mcpHandler(url, q, p, body(), out, err);
    }

    // ---- registration
    if (R("POST", "/platform/oauth/apps/register")) {
      if (hit(fails.register, 600000, 100)) return out(429, { message: "Too many registrations" }, { "retry-after": "60" });
      const b = body();
      const bad = (error, d) => out(400, { message: d, error, error_description: d });
      if (!b || typeof b.client_name !== "string" || !b.client_name.trim() || b.client_name.length > 100) return bad("invalid_client_metadata", "client_name");
      const uris = b.redirect_uris;
      if (!Array.isArray(uris) || uris.length < 1 || uris.length > 10) return bad("invalid_redirect_uri", "redirect_uris");
      for (const u of uris) if (!validRedirect(u)) return bad("invalid_redirect_uri", "redirect_uri not allowed");
      if ((b.grant_types ?? ["authorization_code"]).some((g) => !["authorization_code", "refresh_token"].includes(g))) return bad("invalid_client_metadata", "grant_types");
      if ((b.response_types ?? ["code"]).join() !== "code") return bad("invalid_client_metadata", "response_types");
      const method = b.token_endpoint_auth_method ?? "client_secret_basic";
      if (!["none", "client_secret_basic", "client_secret_post"].includes(method)) return bad("invalid_client_metadata", "token_endpoint_auth_method");
      let scopes = ADVERTISED;
      if (b.scope) { scopes = b.scope.split(/\s+/).filter((s) => ADVERTISED.includes(s)); if (!scopes.length) return bad("invalid_client_metadata", "scope"); }
      const id = uuid(); const secret = "sba_" + hex(32);
      apps.set(id, { id, type: "dynamic", name: b.client_name.replace(/[\u0000-\u001f]/g, "").trim(), website: "", uris, scopes, method, secrets: [{ id: uuid(), hash: sha(secret) }], created: now() });
      return out(201, { id, client_id: id, client_secret: secret, client_secret_expires_at: 0, redirect_uris: uris, client_name: b.client_name, grant_types: b.grant_types ?? ["authorization_code"], response_types: ["code"], scope: scopes.join(" "), token_endpoint_auth_method: method });
    }

    // ---- authorize
    if (R("GET", "/v1/oauth/authorize")) {
      const page = (status, msg) => out(status, `<!doctype html><title>Authorization error</title><p>${msg.replace(/[<>&"']/g, (c) => `&#${c.charCodeAt(0)};`)}</p>`, { "content-type": "text/html; charset=utf-8", "content-security-policy": "default-src 'none'; frame-ancestors 'none'", "cache-control": "no-store" });
      const app = apps.get(q.get("client_id") ?? "");
      if (!app || app.deleted) return page(422, "Unknown client");
      const reqUri = q.get("redirect_uri") ?? "";
      if (!redirectMatch(app.uris, reqUri)) return page(400, `The redirect URI ${reqUri} is not registered`);
      const back = (error, d) => { const u = new URL(reqUri); u.searchParams.set("error", error); u.searchParams.set("error_description", d); if (q.get("state")) u.searchParams.set("state", q.get("state")); u.searchParams.set("iss", issuer); res.writeHead(302, { location: u.toString(), "cache-control": "no-store" }); res.end(); };
      if (q.get("response_type") !== "code") return back("unsupported_response_type", "response_type");
      if (q.get("response_mode") && q.get("response_mode") !== "query") return back("invalid_request", "response_mode");
      const ch = q.get("code_challenge"), cm = q.get("code_challenge_method");
      if (app.type === "dynamic" && !ch) return back("invalid_request", "PKCE is required");
      if (ch || cm) { if (cm !== "S256") return back("invalid_request", "code_challenge_method"); if (!/^[A-Za-z0-9._~-]{43,128}$/.test(ch ?? "")) return back("invalid_request", "code_challenge"); }
      const asked = q.get("scope") ? q.get("scope").split(/\s+/).filter(Boolean) : app.scopes;
      const scopes = asked.filter((s) => app.scopes.includes(s));
      if (!scopes.length) return back("invalid_scope", "scope");
      let resource = q.get("resource") ?? "";
      if (resource) { resource = resource.replace(/[?#].*$/, "").replace(/\/$/, ""); if (resource !== `${issuer}/mcp` && resource !== issuer) return back("invalid_target", "resource"); }
      const hint = q.get("organization_slug") ?? "";
      if (hint && !/^[\w-]+$/.test(hint)) return back("invalid_request", "organization_slug");
      if (hit(fails.authorize, 60000, 60)) return out(429, { message: "slow down" }, { "retry-after": "10" });
      const id = uuid();
      auths.set(id, { id, app: app.id, redirect: reqUri, scopes, state: q.get("state") ?? "", challenge: ch ?? "", resource, hint, expires: now() + 600000, status: "pending" });
      res.writeHead(302, { location: `${dashboard}/authorize?auth_id=${id}${hint ? `&organization_slug=${hint}` : ""}`, "cache-control": "no-store", "referrer-policy": "no-referrer" });
      return res.end();
    }

    // ---- consent
    if ((m = R("GET", "/platform/oauth/authorizations/:id"))) {
      const p = principal(); if (!p || p.via !== "jwt") return err(401, "Unauthorized");
      if (!Object.values(p.user.roles).some((r) => r === "owner" || r === "administrator")) return err(403, "Forbidden");
      const a = auths.get(m.id); if (!a) return err(404, "Not found");
      const app = apps.get(a.app);
      return out(200, { name: app.name, website: app.website, domain: new URL(a.redirect).hostname, redirect_uri: a.redirect, expires_at: new Date(a.expires).toISOString(), scopes: a.scopes, registration_type: app.type, ...(a.approvedAt ? { approved_at: new Date(a.approvedAt).toISOString(), approved_organization_slug: a.approvedOrg } : {}) });
    }
    if ((m = R("POST", "/platform/organizations/:slug/oauth/authorizations/:id")) || (m = R("DELETE", "/platform/organizations/:slug/oauth/authorizations/:id"))) {
      const p = principal(); if (!p || p.via !== "jwt") return err(401, "Unauthorized");
      const org = orgBySlug(m.slug); const r = org && roleIn(p, org.id);
      if (r !== "owner" && r !== "administrator") return err(403, "Forbidden");
      const a = auths.get(m.id); if (!a) return err(404, "Not found");
      if (req.method === "POST" && a.hint && a.hint !== m.slug) return err(403, "The request names another organization");
      if (a.status !== "pending") return err(409, "Already decided");
      if (a.expires < now()) return err(410, "Expired");
      if (req.method === "DELETE") { a.status = "declined"; return out(200, { id: a.id }); }
      a.status = "approved"; a.org = org.id; a.user = p.user.id; a.approvedAt = now(); a.approvedOrg = org.slug; a.code = "sbc_" + hex(32); a.codeExpires = now() + 60000;
      const u = new URL(a.redirect); u.searchParams.set("code", a.code); if (a.state) u.searchParams.set("state", a.state); u.searchParams.set("iss", issuer);
      return out(201, { url: u.toString() }, { "cache-control": "no-store" });
    }

    // ---- token and revoke
    if (R("POST", "/v1/oauth/token")) return tokenEndpoint(ctype, raw, req, out, oerr);
    if (R("POST", "/v1/oauth/revoke")) {
      let b = {}; if (ctype.includes("json")) b = body() ?? {}; else b = Object.fromEntries(new URLSearchParams(raw));
      let id = b.client_id, secret = b.client_secret;
      const basic = (req.headers.authorization ?? "").match(/^Basic (.+)$/i)?.[1];
      if (basic) { const [i, s] = Buffer.from(basic, "base64").toString().split(":"); id = decodeURIComponent(i); secret = decodeURIComponent(s ?? ""); }
      const app = apps.get(id ?? "");
      if (!app || !app.secrets.some((s) => s.hash === sha(secret ?? ""))) return oerr(401, "invalid_client", "Client authentication failed");
      const t = tokens.get(b.refresh_token ?? b.token ?? "");
      if (t && grants[t.grant].app === app.id) revoke(grants[t.grant], "client");
      res.writeHead(204); return res.end();
    }

    // ---- the Management API
    const scoped = (scope) => { const p = principal(); if (!p || (p.via === "jwt" && false)) return null; if (p.via === "oauth") { if (scope === null) { err(403, "This operation is not available to OAuth tokens"); return false; } if (!p.scopes.includes(scope)) { err(403, "Forbidden", { "www-authenticate": `Bearer error="insufficient_scope", scope="${scope}", resource_metadata="${issuer}/.well-known/oauth-protected-resource/mcp"` }); return false; } } return p; };
    const need = (scope) => { const p = scoped(scope); if (p === false) return false; if (!p) { err(401, "Unauthorized"); return false; } return p; };
    if (R("GET", "/v1/profile")) { const p = need(null); return p ? out(200, { email: p.user.email }) : undefined; }
    if (R("POST", "/v1/organizations")) { const p = need(null); return p ? err(400, "name required") : undefined; }
    if (R("GET", "/v1/organizations")) { const p = need("organizations:read"); return p ? out(200, orgs.filter((o) => p.orgs.includes(o.id) && p.user.roles[o.id]).map((o) => ({ id: o.slug, slug: o.slug, name: o.name }))) : undefined; }
    if (R("GET", "/v1/projects")) { const p = need("projects:read"); return p ? out(200, projects.filter((x) => roleIn(p, x.org)).map((x) => ({ id: x.ref, ref: x.ref, organization_id: String(x.org) }))) : undefined; }
    if ((m = R("GET", "/v1/projects/:ref"))) { const p = need("projects:read"); if (!p) return; const x = projects.find((y) => y.ref === m.ref); return x && roleIn(p, x.org) ? out(200, { id: x.ref, ref: x.ref }) : err(403, "Forbidden"); }
    if ((m = R("GET", "/v1/projects/:ref/secrets"))) { const p = need("secrets:read"); if (!p) return; const x = projects.find((y) => y.ref === m.ref); return x && roleIn(p, x.org) ? out(200, []) : err(403, "Forbidden"); }
    if ((m = R("POST", "/v1/projects/:ref/database/query")) || (m = R("POST", "/v1/projects/:ref/database/query/read-only")) || (m = R("POST", "/v1/projects/:ref/database/migrations"))) {
      const ro = url.pathname.endsWith("/read-only"), mig = url.pathname.endsWith("/migrations");
      const p = need(ro ? "database:read" : "database:write"); if (!p) return;
      const x = projects.find((y) => y.ref === m.ref); const r = x && roleIn(p, x.org);
      if (!r) return err(403, "Forbidden");
      const sql = String(body()?.query ?? "");
      const writes = /\b(insert|update|delete|create|drop|alter)\b/i.test(sql);
      if (mig && r === "read-only") return err(403, "Forbidden");
      if ((ro || r === "read-only") && writes) return err(400, "cannot execute in a read-only transaction");
      return out(mig ? 200 : 201, [{ one: 1 }]);
    }

    // ---- organization apps
    const appsRoute = (method, p) => R(method, `/platform/organizations/:slug/oauth/apps${p}`);
    const adminOf = () => { const p = principal(); if (!p || p.via !== "jwt") { err(401, "Unauthorized"); return false; } const org = orgBySlug(m.slug); const r = org && roleIn(p, org.id); if (r !== "owner" && r !== "administrator") { err(403, "Forbidden"); return false; } return { p, org }; };
    if ((m = appsRoute("GET", ""))) {
      const a = adminOf(); if (!a) return;
      if (q.get("type") === "published") return out(200, [...apps.values()].filter((x) => x.type === "manual" && x.org === a.org.id && !x.deleted).map((x) => ({ id: x.id, app_id: x.id, client_id: x.id, name: x.name, website: x.website, redirect_uris: x.uris, scopes: x.scopes })));
      const live = grants.filter((g) => !g.revoked && g.org === a.org.id);
      const ids = [...new Set(live.map((g) => g.app))];
      return out(200, ids.map((id) => { const x = apps.get(id); return { id, app_id: id, client_id: id, name: x.name, website: x.website, registration_type: x.type, redirect_uris: x.uris, scopes: [...new Set(live.filter((g) => g.app === id).flatMap((g) => g.scopes))].filter((s) => x.scopes.includes(s)), authorized_at: new Date(Math.max(...live.filter((g) => g.app === id).map((g) => g.created))).toISOString() }; }));
    }
    if ((m = appsRoute("POST", ""))) {
      const a = adminOf(); if (!a) return;
      const b = body();
      if (!b?.name || !Array.isArray(b.scopes) || b.scopes.some((s) => !ALL24.includes(s)) || !Array.isArray(b.redirect_uris) || b.redirect_uris.some((u) => !validRedirect(u))) return err(400, "invalid app");
      const id = uuid(); const secret = "sba_" + hex(32);
      apps.set(id, { id, type: "manual", org: a.org.id, name: b.name, website: b.website, uris: b.redirect_uris, scopes: b.scopes, method: "client_secret_basic", secrets: [{ id: uuid(), hash: sha(secret), alias: `${secret.slice(0, 8)}********` }], created: now() });
      return out(201, { id, client_id: id, client_secret: secret, client_secret_expires_at: 0, redirect_uris: b.redirect_uris });
    }
    if ((m = appsRoute("PUT", "/:id")) || (m = appsRoute("DELETE", "/:id")) || (m = appsRoute("POST", "/:id/revoke"))) {
      const a = adminOf(); if (!a) return;
      const x = apps.get(m.id);
      const revokeApp = url.pathname.endsWith("/revoke");
      if (!x || x.deleted || (!revokeApp && (x.type !== "manual" || x.org !== a.org.id))) return err(404, "Not found");
      if (revokeApp) { for (const g of grants) if (g.app === x.id && g.org === a.org.id && !g.revoked) revoke(g, "admin"); return out(201, { id: x.id, name: x.name, website: x.website }); }
      if (req.method === "PUT") { const b = body(); Object.assign(x, { name: b.name, website: b.website, scopes: b.scopes, uris: b.redirect_uris }); return out(200, { id: x.id, client_id: x.id, created_at: new Date(x.created).toISOString(), name: x.name, website: x.website, redirect_uris: x.uris }); }
      x.deleted = true; for (const g of grants) if (g.app === x.id) revoke(g, "app_deleted");
      return out(200, { id: x.id, name: x.name, website: x.website, created_at: new Date(x.created).toISOString(), client_id: x.id, redirect_uris: x.uris });
    }
    if ((m = appsRoute("GET", "/:app/client-secrets")) || (m = appsRoute("POST", "/:app/client-secrets")) || (m = appsRoute("DELETE", "/:app/client-secrets/:sid"))) {
      const a = adminOf(); if (!a) return;
      const x = apps.get(m.app);
      if (!x || x.type !== "manual" || x.org !== a.org.id || x.deleted) return err(404, "Not found");
      if (req.method === "GET") return out(200, { client_secrets: x.secrets.map((s) => ({ id: s.id, oauth_app_id: x.id, client_secret_alias: s.alias, created_at: new Date(x.created).toISOString() })) });
      if (req.method === "POST") { const secret = "sba_" + hex(32); const s = { id: uuid(), hash: sha(secret), alias: `${secret.slice(0, 8)}********` }; x.secrets.push(s); return out(201, { id: s.id, oauth_app_id: x.id, client_secret_alias: s.alias, client_secret: secret, created_at: new Date().toISOString() }); }
      const i = x.secrets.findIndex((s) => s.id === m.sid); if (i < 0) return err(404, "Not found");
      x.secrets.splice(i, 1); return out(200, { id: m.sid });
    }

    // ---- members and organizations
    if ((m = R("PATCH", "/platform/organizations/:slug/members/:uid")) || (m = R("DELETE", "/platform/organizations/:slug/members/:uid"))) {
      const p = principal(); const org = orgBySlug(m.slug); if (!p || p.via !== "jwt") return err(401, "Unauthorized");
      if (roleIn(p, org.id) !== "owner") return err(403, "Forbidden");
      const u = userById(m.uid); if (!u) return err(404, "Not found");
      if (req.method === "DELETE") { delete u.roles[org.id]; return out(200, {}); }
      u.roles[org.id] = ROLE_IDS[body().role_id]; return out(200, {});
    }
    if (R("GET", "/platform/profile")) { const p = principal(); return p && p.via === "jwt" ? out(200, { id: 1 }) : err(401, "Unauthorized"); }

    return err(404, "Not Found");
  });

  // ---- helpers that need the server's state
  function corsOpen(path) { return path.startsWith("/.well-known/") || path === "/v1/oauth/token" || path === "/v1/oauth/revoke" || path === "/platform/oauth/apps/register" || path === "/mcp"; }
  function validRedirect(u) {
    let x; try { x = new URL(u); } catch { return false; }
    if (x.hash || x.username || x.password || u.length > 2048) return false;
    if (x.protocol === "https:") return true;
    return x.protocol === "http:" && ["localhost", "127.0.0.1", "[::1]"].includes(x.hostname);
  }
  function redirectMatch(registered, asked) {
    if (registered.includes(asked)) return true;
    let a; try { a = new URL(asked); } catch { return false; }
    return registered.some((r) => { const x = new URL(r); return x.protocol === "http:" && ["localhost", "127.0.0.1", "[::1]"].includes(x.hostname) && a.protocol === "http:" && a.hostname === x.hostname && a.pathname === x.pathname && a.search === x.search; });
  }
  function revoke(g, reason) { if (!g.revoked) g.revoked = reason; }

  async function tokenEndpoint(ctype, raw, req, out, oerr) {
    // The failure limiter: 30 failures a minute per address; counted on every refusal.
    const recent = fails.token.filter((t) => now() - t < 60000);
    fails.token.length = 0; fails.token.push(...recent);
    if (recent.length >= 30) return out(429, { message: "Too many failed requests" }, { "retry-after": "30" });
    const fail = (status, error, message, h) => { fails.token.push(now()); return oerr(status, error, message, h); };
    if (!ctype.includes("application/x-www-form-urlencoded")) return fail(400, "invalid_request", "form body required");
    const f = Object.fromEntries(new URLSearchParams(raw));
    let id = f.client_id, secret = f.client_secret, usedBasic = false;
    const basic = (req.headers.authorization ?? "").match(/^Basic (.+)$/i)?.[1];
    if (basic) {
      const [i, s] = Buffer.from(basic, "base64").toString().split(":"); usedBasic = true;
      if ((f.client_id && f.client_id !== decodeURIComponent(i)) || (f.client_secret && f.client_secret !== decodeURIComponent(s ?? ""))) return fail(400, "invalid_request", "credentials in the header and the body differ");
      id = decodeURIComponent(i); secret = decodeURIComponent(s ?? "");
    }
    const app = apps.get(id ?? "");
    const badClient = () => fail(401, "invalid_client", "Client authentication failed", usedBasic ? { "www-authenticate": "Basic" } : {});
    if (!app || app.deleted) return badClient();
    if (secret) { if (!app.secrets.some((s) => s.hash === sha(secret))) return badClient(); }
    else if (app.type === "manual" || f.grant_type !== "authorization_code") return badClient();
    const gt = f.grant_type;
    if (gt === "authorization_code") {
      const a = [...auths.values()].find((x) => x.code === f.code && x.app === app.id);
      if (!a) return fail(400, "invalid_grant", "invalid code");
      if (a.status === "exchanged") { if (a.grant !== undefined) revoke(grants[a.grant], "code_reuse"); return fail(400, "invalid_grant", "code already used"); }
      const burn = () => { a.status = "exchanged"; return fail(400, "invalid_grant", "invalid code"); };
      if (a.status !== "approved" || a.codeExpires < now() || f.redirect_uri !== a.redirect) return burn();
      if (f.resource && f.resource !== a.resource) return fail(400, "invalid_target", "resource");
      if (a.challenge) { if (!/^[A-Za-z0-9._~-]{43,128}$/.test(f.code_verifier ?? "") || crypto.createHash("sha256").update(f.code_verifier).digest("base64url") !== a.challenge) return burn(); }
      const user = userById(a.user);
      if (!user || !user.roles[a.org]) return burn();
      for (const g of grants) if (g.app === app.id && g.user === a.user && g.org === a.org && !g.revoked) revoke(g, "superseded");
      const g = { id: grants.length, app: app.id, user: a.user, org: a.org, scopes: a.scopes, resource: a.resource, created: now(), revoked: "" };
      grants.push(g); a.grant = g.id; a.status = "exchanged";
      return out(200, issue(g, g.scopes), { "cache-control": "no-store", pragma: "no-cache" });
    }
    if (gt === "refresh_token") {
      const t = tokens.get(f.refresh_token ?? "");
      if (!t || t.kind !== "refresh" || t.expires < now()) return fail(400, "invalid_grant", "invalid refresh token");
      const g = grants[t.grant];
      if (g.app !== app.id || g.revoked) return fail(400, "invalid_grant", "invalid refresh token");
      if (t.used && now() - t.used > 10000) { revoke(g, "refresh_reuse"); return fail(400, "invalid_grant", "refresh token reuse"); }
      t.used ??= now();
      let scopes = g.scopes;
      if (f.scope) { const want = f.scope.split(/\s+/).filter(Boolean); if (want.some((s) => !g.scopes.includes(s))) return fail(400, "invalid_scope", "scope"); scopes = want; }
      if (f.resource && f.resource !== g.resource) return fail(400, "invalid_target", "resource");
      return out(200, issue(g, scopes), { "cache-control": "no-store", pragma: "no-cache" });
    }
    return fail(400, "unsupported_grant_type", "unsupported grant type");
  }
  function issue(g, scopes) {
    const access = "sbp_oauth_" + hex(20), refresh = "sbr_" + hex(32);
    tokens.set(access, { kind: "access", grant: g.id, expires: now() + 3600000 });
    tokens.set(refresh, { kind: "refresh", grant: g.id, expires: now() + 90 * 86400000 });
    return { access_token: access, token_type: "Bearer", expires_in: 3600, refresh_token: refresh, scope: scopes.join(" ") };
  }

  function mcpHandler(url, q, p, b, out, err) {
    const ref = q.get("project_ref"), ro = q.get("read_only"), features = q.get("features");
    if (ref && !/^[a-z]{20}$/.test(ref)) return err(400, "invalid project_ref");
    if (ro && ro !== "true" && ro !== "false") return err(400, "invalid read_only");
    const project = ["execute_sql", "list_tables", "get_project_url"];
    const writes = ["apply_migration"];
    const account = ["list_organizations", "list_projects", "get_cost", "confirm_cost", "create_project"];
    let tools = features === "docs" ? ["search_docs"] : [...project, ...(ro === "true" ? [] : writes), ...(ref ? [] : account)];
    const text = (t, isError = false) => ({ jsonrpc: "2.0", id: b.id, result: { content: [{ type: "text", text: t }], isError } });
    if (b.method === "initialize") return out(200, { jsonrpc: "2.0", id: b.id, result: { protocolVersion: b.params?.protocolVersion ?? "2025-06-18", capabilities: {}, serverInfo: { name: "fake", version: "0" } } });
    if (b.method === "tools/list") return out(200, { jsonrpc: "2.0", id: b.id, result: { tools: tools.map((name) => ({ name })) } });
    if (b.method === "tools/call") {
      const n = b.params.name;
      if (!tools.includes(n)) return out(200, { jsonrpc: "2.0", id: b.id, error: { code: -32602, message: "unknown tool" } });
      if (n === "execute_sql") return out(200, text(ro === "true" && /\b(insert|create|drop)\b/i.test(b.params.arguments.query) ? "cannot execute in a read-only transaction" : '[{"one":1}]', ro === "true" && /\b(insert|create|drop)\b/i.test(b.params.arguments.query)));
      if (n === "get_project_url") return out(200, text(`http://${ref}.api.${domain}`));
      if (n === "list_organizations") return out(200, text(JSON.stringify(orgs.filter((o) => p.orgs.includes(o.id)).map((o) => o.slug))));
      if (n === "list_projects") return out(200, text(JSON.stringify(projects.filter((x) => p.orgs.includes(x.org)).map((x) => x.ref))));
      if (n === "create_project") return out(200, text("Cost confirmation ID does not match", true));
      return out(200, text("ok"));
    }
    return out(202, "");
  }

  function cliRoute(url, q, out, err) {
    const org = orgs[0];
    switch (url.pathname) {
      case "/__cli/invite": { const role = q.get("role"), email = q.get("email"); const token = "sbi_" + hex(24); invites.set(token, { email, role, orgId: org.id }); return out(200, `${issuer}/claim#token=${token}&email=${encodeURIComponent(email)}\n`, { "content-type": "text/plain" }); }
      case "/__cli/remove": { const u = users.get(q.get("email")); if (!u) return err(404, "no such user"); u.removed = true; for (const g of grants) if (g.user === u.id) revoke(g, "user_removed"); return out(200, "removed\n", { "content-type": "text/plain" }); }
      case "/__cli/grants": {
        const sel = (g) => (!q.get("user") || userById(g.user)?.email === q.get("user")) && (!q.get("app") || g.app === q.get("app")) && !g.revoked;
        if (q.get("revoke")) { let n = 0; for (const g of grants) if (sel(g)) { revoke(g, "operator"); n++; } return out(200, `revoked ${n}\n`, { "content-type": "text/plain" }); }
        return out(200, grants.filter(sel).map((g) => `${g.id}\t${apps.get(g.app).name}\t${userById(g.user)?.email}\t${orgs.find((o) => o.id === g.org).slug}\n`).join(""), { "content-type": "text/plain" });
      }
    }
    return err(404, "unknown cli route");
  }

  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  return {
    port: server.address().port, issuer, dashboard, domain, pat, org: "default", org2: "second", ref: projects[0].ref, ref2: projects[1].ref,
    owner: { email: owner.email, password: owner.password }, close: () => new Promise((r) => { server.closeAllConnections?.(); server.close(r); }),
  };
}
