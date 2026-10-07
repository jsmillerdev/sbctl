package registry

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestMemoryDomains(t *testing.T) { testDomains(t, NewMemory()) }

// TestPostgresDomains runs the same checks against a real database (see TestPostgres).
func TestPostgresDomains(t *testing.T) {
	dsn := os.Getenv("SUPAVISE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SUPAVISE_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	r, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Pool().Exec(ctx, `truncate supavise.projects, supavise.organizations cascade`); err != nil {
		t.Fatal(err)
	}
	testDomains(t, r)
}

// testDomains is the shared conformance check of the DomainStore of a registry.
func testDomains(t *testing.T, r Registry) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d := Domains(r)
	if d == nil {
		t.Fatal("registry has no DomainStore")
	}
	org, err := r.CreateOrganization(ctx, "dom-org", "Domains")
	if err != nil {
		t.Fatal(err)
	}
	const a, b = "aaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbb"
	for _, ref := range []string{a, b} {
		if err := r.CreateProject(ctx, &Project{Ref: ref, OrgID: org.ID, Name: ref}); err != nil {
			t.Fatal(err)
		}
	}
	routeOf := func(host string) *Route {
		rs, err := r.ListRoutes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range rs {
			if x.Host == host {
				return &x
			}
		}
		return nil
	}

	if _, err := d.GetCustomHostname(ctx, a); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get before put: %v", err)
	}
	if err := d.PutCustomHostname(ctx, &CustomHostname{Ref: a, Hostname: "docs.example.org", Status: HostnameInitiated, Token: "tok-a"}); err != nil {
		t.Fatal(err)
	}
	// A pending claim does not lock the name.
	if err := d.PutCustomHostname(ctx, &CustomHostname{Ref: b, Hostname: "docs.example.org", Status: HostnameInitiated, Token: "tok-b"}); err != nil {
		t.Fatalf("second pending claim: %v", err)
	}
	got, err := d.GetCustomHostname(ctx, a)
	if err != nil || got.Hostname != "docs.example.org" || got.Status != HostnameInitiated || got.Token != "tok-a" || got.CNAMEOK || got.TXTOK {
		t.Fatalf("get: %+v %v", got, err)
	}

	// Verified: the claim holds the name; the other claim cannot get there.
	now := time.Now()
	got.Status, got.CNAMEOK, got.TXTOK, got.VerifiedAt = HostnameOriginReady, true, true, &now
	if err := d.UpdateCustomHostname(ctx, got); err != nil {
		t.Fatal(err)
	}
	other, _ := d.GetCustomHostname(ctx, b)
	other.Status = HostnameOriginReady
	if err := d.UpdateCustomHostname(ctx, other); !errors.Is(err, ErrConflict) {
		t.Fatalf("verifying a held name: %v", err)
	}
	if err := d.PutCustomHostname(ctx, &CustomHostname{Ref: b, Hostname: "docs.example.org", Status: HostnameInitiated, Token: "tok-b2"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("claiming a held name: %v", err)
	}
	// A verified claim whose proof is old stops holding the name; a fresh one keeps it.
	if n, err := d.ReleaseStaleClaims(ctx, "docs.example.org", now.Add(-time.Hour)); err != nil || n != 0 {
		t.Fatalf("releasing a fresh claim: %d %v", n, err)
	}
	if n, err := d.ReleaseStaleClaims(ctx, "elsewhere.example.org", now.Add(time.Hour)); err != nil || n != 0 {
		t.Fatalf("releasing another hostname: %d %v", n, err)
	}
	if n, err := d.ReleaseStaleClaims(ctx, "docs.example.org", now.Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("releasing a stale claim: %d %v", n, err)
	}
	if rel, err := d.GetCustomHostname(ctx, a); err != nil || rel.Status != HostnameInitiated || rel.CNAMEOK || rel.TXTOK || rel.VerifiedAt != nil {
		t.Fatalf("released claim: %+v %v", rel, err)
	}
	if err := d.UpdateCustomHostname(ctx, got); err != nil { // a proves it again
		t.Fatalf("verifying again: %v", err)
	}
	// A stale writer (the claim was replaced meanwhile) is refused.
	stale := *got
	stale.Hostname = "elsewhere.example.org"
	if err := d.UpdateCustomHostname(ctx, &stale); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale update: %v", err)
	}
	if err := d.UpdateCustomHostname(ctx, &CustomHostname{Ref: "cccccccccccccccccccc", Hostname: "x.example.org", Status: HostnameInitiated}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update without a claim: %v", err)
	}

	// Activation needs a verified claim, adds the route, and cannot take a routed name.
	if _, err := d.ActivateCustomHostname(ctx, b); !errors.Is(err, ErrConflict) {
		t.Fatalf("activating an unverified claim: %v", err)
	}
	act, err := d.ActivateCustomHostname(ctx, a)
	if err != nil || act.Status != HostnameActive || act.ActivatedAt == nil {
		t.Fatalf("activate: %+v %v", act, err)
	}
	if rt := routeOf("docs.example.org"); rt == nil || rt.Ref != a || rt.Kind != RouteCustom {
		t.Fatalf("route after activation: %+v", rt)
	}
	if _, err := d.ActivateCustomHostname(ctx, a); !errors.Is(err, ErrConflict) {
		t.Fatalf("activating twice: %v", err)
	}
	if err := d.PutCustomHostname(ctx, &CustomHostname{Ref: a, Hostname: "new.example.org", Status: HostnameInitiated, Token: "x"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("replacing an active claim: %v", err)
	}
	if err := d.UpdateCustomHostname(ctx, act); !errors.Is(err, ErrConflict) {
		t.Fatalf("updating an active claim: %v", err)
	}
	list, err := d.ListCustomHostnames(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %+v %v", list, err)
	}

	if err := d.DeleteCustomHostname(ctx, a); err != nil {
		t.Fatal(err)
	}
	if routeOf("docs.example.org") != nil {
		t.Fatal("route survived the delete")
	}
	if err := d.DeleteCustomHostname(ctx, a); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	// The pending claim of b is untouched, and can now be verified.
	other, err = d.GetCustomHostname(ctx, b)
	if err != nil || other.Status != HostnameInitiated {
		t.Fatalf("b: %+v %v", other, err)
	}

	// Vanity subdomains.
	if _, err := d.GetVanitySubdomain(ctx, a); !errors.Is(err, ErrNotFound) {
		t.Fatalf("vanity before put: %v", err)
	}
	if err := d.PutVanitySubdomain(ctx, a, "acme", "acme.api.example.com"); err != nil {
		t.Fatal(err)
	}
	if owner, err := d.VanitySubdomainOwner(ctx, "acme"); err != nil || owner != a {
		t.Fatalf("owner: %q %v", owner, err)
	}
	if err := d.PutVanitySubdomain(ctx, b, "acme", "acme.api.example.com"); !errors.Is(err, ErrConflict) {
		t.Fatalf("taking a vanity name: %v", err)
	}
	if rt := routeOf("acme.api.example.com"); rt == nil || rt.Ref != a || rt.Kind != RouteVanity {
		t.Fatalf("vanity route: %+v", rt)
	}
	// A vanity host cannot take over a route that is not the project's own vanity route.
	if err := d.PutVanitySubdomain(ctx, b, "beta", "acme.api.example.com"); !errors.Is(err, ErrConflict) {
		t.Fatalf("taking another project's route: %v", err)
	}
	if v, err := d.GetVanitySubdomain(ctx, b); err == nil {
		t.Fatalf("a failed put left a vanity row: %+v", v)
	}
	// Renaming moves the route.
	if err := d.PutVanitySubdomain(ctx, a, "acme2", "acme2.api.example.com"); err != nil {
		t.Fatal(err)
	}
	if routeOf("acme.api.example.com") != nil || routeOf("acme2.api.example.com") == nil {
		t.Fatal("rename did not move the route")
	}
	if _, err := d.VanitySubdomainOwner(ctx, "acme"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("released name: %v", err)
	}
	if err := d.DeleteVanitySubdomain(ctx, a); err != nil {
		t.Fatal(err)
	}
	if routeOf("acme2.api.example.com") != nil {
		t.Fatal("vanity route survived the delete")
	}
	if err := d.DeleteVanitySubdomain(ctx, a); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second vanity delete: %v", err)
	}

	// Deleting a project takes its domains with it.
	if err := d.PutVanitySubdomain(ctx, b, "beta", "beta.api.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteProject(ctx, b); err != nil {
		t.Fatal(err)
	}
	if hs, _ := d.ListCustomHostnames(ctx); len(hs) != 0 {
		t.Fatalf("hostnames after project delete: %+v", hs)
	}
	if _, err := d.VanitySubdomainOwner(ctx, "beta"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("vanity after project delete: %v", err)
	}
	if routeOf("beta.api.example.com") != nil {
		t.Fatal("route after project delete")
	}
}
