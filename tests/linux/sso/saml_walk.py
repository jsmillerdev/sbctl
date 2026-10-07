#!/usr/bin/env python3
"""Walks a browser through SAML single sign-on against a GoTrue behind sbctl's proxy.

    saml_walk.py --api-base http://127.0.0.1 --api-host api.sbctl.test --email alice@acme.test \
                 --user alice --password alicepass [--apikey KEY] [--redirect-to URL]

What a browser does when somebody clicks "Continue with SSO" (Studio, or supabase-js for a
project's end user), without the browser:

 1. POST <api-base>/auth/v1/sso  {domain, redirect_to, skip_http_redirect: true}    (Host: <api-host>)
    GoTrue answers the identity provider's address, with the AuthnRequest in it.
 2. GET that address and follow the identity provider's redirects until a login form shows (a
    form with a password field; the "email" field of the test IdP is accepted too), fill it in
    (--user, --password) and submit it, with the cookies the provider set.
 3. The provider answers a page that posts the signed assertion (SAMLResponse, RelayState) to
    the ACS URL. POST it there, through the proxy again (<api-base> with the ACS URL's host as
    the Host header), without following the redirect.
 4. GoTrue answers a redirect to the dashboard (or the project's site) with the session in the
    fragment, or the error in the query. Print it.

Prints one JSON object: {"status": <ACS status>, "location": ..., "access_token": ..., "error": ...,
"idp_url": ..., "acs_url": ...}. Exit status 0 when the walk reached the ACS, 2 when it did not (the
message says where it stopped). Only the Python 3 standard library.
"""
import argparse
import html
import http.cookiejar
import json
import sys
import urllib.error
import urllib.parse
import urllib.request
from html.parser import HTMLParser


class Form:
    def __init__(self, attrs):
        self.action = attrs.get("action", "")
        self.method = attrs.get("method", "get").lower()
        self.inputs = []  # (name, value, type)


class Forms(HTMLParser):
    def __init__(self):
        super().__init__(convert_charrefs=True)
        self.forms = []

    def handle_starttag(self, tag, attrs):
        a = dict(attrs)
        if tag == "form":
            self.forms.append(Form(a))
        elif tag in ("input", "button") and self.forms and a.get("name"):
            self.forms[-1].inputs.append((a["name"], a.get("value") or "", (a.get("type") or "text").lower()))


def forms_of(page):
    p = Forms()
    p.feed(page)
    return p.forms


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *a, **k):
        return None


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--api-base", required=True, help="where the proxy listens, e.g. http://127.0.0.1")
    ap.add_argument("--api-host", required=True, help="the Host of the API (api.<domain>, or <ref>.api.<domain>)")
    ap.add_argument("--email", required=True)
    ap.add_argument("--user", help="the login at the identity provider (default: the email)")
    ap.add_argument("--password", default="")
    ap.add_argument("--apikey", default="", help="a project's publishable key (the project's /auth/v1/sso needs one)")
    ap.add_argument("--redirect-to", default="")
    ap.add_argument("--max-steps", type=int, default=12)
    ap.add_argument("--verbose", action="store_true")
    args = ap.parse_args()

    jar = http.cookiejar.CookieJar()
    opener = urllib.request.build_opener(NoRedirect, urllib.request.HTTPCookieProcessor(jar))

    def log(*a):
        if args.verbose:
            print(*a, file=sys.stderr)

    def req(method, url, data=None, headers=None):
        """One request, redirects not followed. Returns (status, headers, body, final url)."""
        r = urllib.request.Request(url, data=data, method=method, headers=headers or {})
        try:
            resp = opener.open(r, timeout=60)
        except urllib.error.HTTPError as e:
            resp = e
        body = resp.read().decode("utf-8", "replace")
        return resp.status, resp.headers, body, url

    def stop(msg, **extra):
        print(json.dumps(dict(extra, stopped=msg)))
        sys.exit(2)

    domain = args.email.split("@", 1)[1]
    hdr = {"Host": args.api_host, "Content-Type": "application/json", "Origin": "http://studio." + args.api_host.split(".", 1)[-1]}
    if args.apikey:
        hdr["apikey"] = args.apikey
    body = {"domain": domain, "skip_http_redirect": True}
    if args.redirect_to:
        body["redirect_to"] = args.redirect_to
    st, _, text, _ = req("POST", args.api_base + "/auth/v1/sso", json.dumps(body).encode(), hdr)
    if st != 200:
        stop("sso answered %d: %s" % (st, text[:300]))
    idp_url = json.loads(text)["url"]
    log("idp:", idp_url)

    url, data, method = idp_url, None, "GET"
    acs = None
    for step in range(args.max_steps):
        st, h, page, cur = req(method, url, data, {"Content-Type": "application/x-www-form-urlencoded"} if data else {})
        log("step", step, method, cur, st)
        if st in (301, 302, 303, 307, 308):
            url, data, method = urllib.parse.urljoin(cur, h.get("Location", "")), None, "GET"
            continue
        if st != 200:
            stop("the identity provider answered %d at %s: %s" % (st, cur, page[:300]))
        fs = forms_of(page)
        post = next((f for f in fs if any(n == "SAMLResponse" for n, _, _ in f.inputs)), None)
        if post:
            acs = (urllib.parse.urljoin(cur, html.unescape(post.action)), post.inputs)
            break
        login = next((f for f in fs if any(t == "password" for _, _, t in f.inputs)), None) or \
            next((f for f in fs if any(n == "email" for n, _, _ in f.inputs)), None)
        if not login:
            stop("no login form and no assertion at %s: %s" % (cur, page[:300]))
        fields = {}
        for n, v, t in login.inputs:
            if t == "password":
                fields[n] = args.password
            elif n == "email":
                fields[n] = args.email
            elif n in ("username", "user", "login", "j_username", "identifier"):
                fields[n] = args.user or args.email
            else:
                fields[n] = v  # hidden state (AuthState, RelayState, ...) and submit buttons
        url = urllib.parse.urljoin(cur, html.unescape(login.action)) if login.action else cur
        method, data = "POST", urllib.parse.urlencode(fields).encode()
    if acs is None:
        stop("the identity provider did not hand back an assertion in %d steps" % args.max_steps)

    acs_url, inputs = acs
    u = urllib.parse.urlparse(acs_url)
    # The ACS URL is the API's public address; the walk reaches it through the proxy.
    target = args.api_base + u.path + (("?" + u.query) if u.query else "")
    form = urllib.parse.urlencode([(n, v) for n, v, _ in inputs]).encode()
    st, h, page, _ = req("POST", target, form, {"Host": u.netloc, "Content-Type": "application/x-www-form-urlencoded"})
    loc = h.get("Location", "")
    out = {"status": st, "location": loc, "idp_url": idp_url, "acs_url": acs_url, "access_token": "", "error": ""}
    lu = urllib.parse.urlparse(loc)
    frag = urllib.parse.parse_qs(lu.fragment)
    query = urllib.parse.parse_qs(lu.query)
    if frag.get("access_token"):
        out["access_token"] = frag["access_token"][0]
    if query.get("error_description") or query.get("error"):
        out["error"] = (query.get("error_description") or query.get("error"))[0]
    if query.get("code"):
        out["code"] = query["code"][0]
    if not loc:
        out["body"] = page[:300]
    print(json.dumps(out))


if __name__ == "__main__":
    main()
