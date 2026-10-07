#!/usr/bin/env python3
"""A SAML 2.0 identity provider for tests: just enough of one to sign a user in to GoTrue.

    python3 saml-idp.py PORT [entity-id-path]

Needs `pip install signxml` (it brings lxml and cryptography). Everything is in memory; the
signing key is made at start. Endpoints:

  GET  /metadata        the IdP's metadata (entity id http://127.0.0.1:PORT/idp, the signing
                        certificate, the redirect binding at /sso)
  GET  /sso             the target of GoTrue's AuthnRequest (HTTP-Redirect binding): answers a
                        login form that posts to /login with the request's parameters
  POST /login           email=<address>&SAMLRequest=...&RelayState=...: answers the page a
                        browser auto-submits to the service provider, with a signed assertion
                        for that address (NameID persistent, the address as the "email"
                        attribute)
  GET  /health          200

It prints "ready" on stdout once it listens. Not for anything but tests: it asks for no password.
"""
import base64
import datetime
import html
import sys
import uuid
import zlib
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import parse_qs, urlparse

from cryptography import x509
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import rsa
from cryptography.x509.oid import NameOID
from lxml import etree
from signxml import XMLSigner, methods

PORT = int(sys.argv[1])
ENTITY = "http://127.0.0.1:%d/%s" % (PORT, sys.argv[2] if len(sys.argv) > 2 else "idp")
SSO = "http://127.0.0.1:%d/sso" % PORT

KEY = rsa.generate_private_key(public_exponent=65537, key_size=2048)
NAME = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, "test idp")])
NOW = datetime.datetime.now(datetime.timezone.utc)
CERT = (
    x509.CertificateBuilder()
    .subject_name(NAME)
    .issuer_name(NAME)
    .public_key(KEY.public_key())
    .serial_number(x509.random_serial_number())
    .not_valid_before(NOW - datetime.timedelta(days=1))
    .not_valid_after(NOW + datetime.timedelta(days=365))
    .sign(KEY, hashes.SHA256())
)
CERT_PEM = CERT.public_bytes(serialization.Encoding.PEM)
CERT_B64 = base64.b64encode(CERT.public_bytes(serialization.Encoding.DER)).decode()
KEY_PEM = KEY.private_bytes(
    serialization.Encoding.PEM, serialization.PrivateFormat.PKCS8, serialization.NoEncryption()
)

NS_P = "urn:oasis:names:tc:SAML:2.0:protocol"
NS_A = "urn:oasis:names:tc:SAML:2.0:assertion"


def metadata():
    return (
        '<?xml version="1.0"?>'
        '<md:EntityDescriptor xmlns:md="urn:oasis:names:tc:SAML:2.0:metadata" entityID="%s">'
        '<md:IDPSSODescriptor WantAuthnRequestsSigned="false" protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol">'
        '<md:KeyDescriptor use="signing"><ds:KeyInfo xmlns:ds="http://www.w3.org/2000/09/xmldsig#"><ds:X509Data>'
        "<ds:X509Certificate>%s</ds:X509Certificate></ds:X509Data></ds:KeyInfo></md:KeyDescriptor>"
        "<md:NameIDFormat>urn:oasis:names:tc:SAML:2.0:nameid-format:persistent</md:NameIDFormat>"
        "<md:NameIDFormat>urn:oasis:names:tc:SAML:1.1:nameid-format:emailAddress</md:NameIDFormat>"
        '<md:SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect" Location="%s"/>'
        '<md:SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-POST" Location="%s"/>'
        "</md:IDPSSODescriptor></md:EntityDescriptor>" % (ENTITY, CERT_B64, SSO, SSO)
    )


def parse_request(saml_request):
    raw = base64.b64decode(saml_request)
    try:
        raw = zlib.decompress(raw, -15)  # HTTP-Redirect binding: raw deflate
    except zlib.error:
        pass  # HTTP-POST binding: plain
    root = etree.fromstring(raw)
    return {
        "id": root.get("ID"),
        "acs": root.get("AssertionConsumerServiceURL"),
        "sp": root.find("{%s}Issuer" % NS_A).text,
    }


def iso(dt):
    return dt.strftime("%Y-%m-%dT%H:%M:%SZ")


def response(req, email):
    now = datetime.datetime.now(datetime.timezone.utc)
    later = now + datetime.timedelta(minutes=5)
    rid, aid = "_" + uuid.uuid4().hex, "_" + uuid.uuid4().hex
    subject = uuid.uuid5(uuid.NAMESPACE_URL, ENTITY + "/" + email)
    xml = (
        '<samlp:Response xmlns:samlp="%(p)s" xmlns:saml="%(a)s" ID="%(rid)s" Version="2.0" IssueInstant="%(now)s" '
        'Destination="%(acs)s" InResponseTo="%(irt)s">'
        "<saml:Issuer>%(idp)s</saml:Issuer>"
        '<samlp:Status><samlp:StatusCode Value="urn:oasis:names:tc:SAML:2.0:status:Success"/></samlp:Status>'
        '<saml:Assertion ID="%(aid)s" Version="2.0" IssueInstant="%(now)s">'
        "<saml:Issuer>%(idp)s</saml:Issuer>"
        '<ds:Signature Id="placeholder" xmlns:ds="http://www.w3.org/2000/09/xmldsig#"/>'
        '<saml:Subject><saml:NameID Format="urn:oasis:names:tc:SAML:2.0:nameid-format:persistent">%(sub)s</saml:NameID>'
        '<saml:SubjectConfirmation Method="urn:oasis:names:tc:SAML:2.0:cm:bearer">'
        '<saml:SubjectConfirmationData NotOnOrAfter="%(later)s" Recipient="%(acs)s" InResponseTo="%(irt)s"/>'
        "</saml:SubjectConfirmation></saml:Subject>"
        '<saml:Conditions NotBefore="%(now)s" NotOnOrAfter="%(later)s"><saml:AudienceRestriction>'
        "<saml:Audience>%(sp)s</saml:Audience></saml:AudienceRestriction></saml:Conditions>"
        '<saml:AuthnStatement AuthnInstant="%(now)s" SessionIndex="%(aid)s"><saml:AuthnContext>'
        "<saml:AuthnContextClassRef>urn:oasis:names:tc:SAML:2.0:ac:classes:PasswordProtectedTransport</saml:AuthnContextClassRef>"
        "</saml:AuthnContext></saml:AuthnStatement>"
        '<saml:AttributeStatement><saml:Attribute Name="email" NameFormat="urn:oasis:names:tc:SAML:2.0:attrname-format:basic">'
        '<saml:AttributeValue xmlns:xs="http://www.w3.org/2001/XMLSchema" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:type="xs:string">%(email)s</saml:AttributeValue>'
        "</saml:Attribute></saml:AttributeStatement>"
        "</saml:Assertion></samlp:Response>"
    ) % {
        "p": NS_P, "a": NS_A, "rid": rid, "aid": aid, "now": iso(now), "later": iso(later), "acs": html.escape(req["acs"], quote=True),
        "irt": html.escape(req["id"], quote=True), "idp": ENTITY, "sp": html.escape(req["sp"]), "sub": subject, "email": html.escape(email),
    }
    root = etree.fromstring(xml.encode())
    assertion = root.find("{%s}Assertion" % NS_A)
    signed = XMLSigner(method=methods.enveloped, signature_algorithm="rsa-sha256", digest_algorithm="sha256",
                       c14n_algorithm="http://www.w3.org/2001/10/xml-exc-c14n#").sign(
        assertion, key=KEY_PEM, cert=CERT_PEM, reference_uri=aid, id_attribute="ID")
    root.replace(assertion, signed)
    return etree.tostring(root)


def page(body):
    return ("<!doctype html><html><body>%s</body></html>" % body).encode()


class H(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        sys.stderr.write("idp: " + fmt % args + "\n")

    def send(self, code, body, ctype="text/html"):
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        u = urlparse(self.path)
        q = parse_qs(u.query)
        if u.path == "/health":
            return self.send(200, b"ok", "text/plain")
        if u.path == "/metadata":
            return self.send(200, metadata().encode(), "application/xml")
        if u.path == "/sso":
            try:
                parse_request(q["SAMLRequest"][0])
            except Exception as e:  # noqa: BLE001
                return self.send(400, ("bad request: %s" % e).encode(), "text/plain")
            return self.send(200, page(
                '<form method="post" action="/login"><input name="email">'
                '<input type="hidden" name="SAMLRequest" value="%s"><input type="hidden" name="RelayState" value="%s">'
                "</form>" % (html.escape(q["SAMLRequest"][0], quote=True), html.escape(q.get("RelayState", [""])[0], quote=True))))
        self.send(404, b"not found", "text/plain")

    def do_POST(self):
        n = int(self.headers.get("Content-Length", "0"))
        form = parse_qs(self.rfile.read(n).decode())
        if self.path != "/login":
            return self.send(404, b"not found", "text/plain")
        try:
            req = parse_request(form["SAMLRequest"][0])
            resp = base64.b64encode(response(req, form["email"][0])).decode()
        except Exception as e:  # noqa: BLE001
            return self.send(400, ("bad request: %s" % e).encode(), "text/plain")
        self.send(200, page(
            '<form method="post" action="%s"><input type="hidden" name="SAMLResponse" value="%s">'
            '<input type="hidden" name="RelayState" value="%s"></form>' % (
                html.escape(req["acs"], quote=True), resp, html.escape(form.get("RelayState", [""])[0], quote=True))))


if __name__ == "__main__":
    srv = HTTPServer(("127.0.0.1", PORT), H)
    print("ready", flush=True)
    srv.serve_forever()
