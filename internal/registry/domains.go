package registry

import (
	"context"
	"time"
)

// Status values of a custom hostname, the ones of hosted's custom-hostname API (and the ones
// Studio's Custom Domains page switches on).
const (
	HostnameNotStarted        = "1_not_started"
	HostnameInitiated         = "2_initiated"          // claimed; DNS not proven yet
	HostnameChallengeVerified = "3_challenge_verified" // the TXT ownership record is there, the hostname does not point at the node yet
	HostnameOriginReady       = "4_origin_setup_completed"
	HostnameActive            = "5_services_reconfigured"
)

// Route kinds written for domains (Route.Kind).
const (
	RouteCustom = "custom"
	RouteVanity = "vanity"
)

// CustomHostname is a project's custom hostname and how far its setup got.
type CustomHostname struct {
	Ref      string
	Hostname string // lower case, no trailing dot
	Status   string
	// Token is the value of the TXT record _supavise-challenge.<Hostname>.
	Token string
	// CNAMEOK and TXTOK are what the last check found.
	CNAMEOK, TXTOK bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
	VerifiedAt     *time.Time
	ActivatedAt    *time.Time
}

// VanitySubdomain is a project's vanity subdomain; its host is <Name>.api.<base domain>.
type VanitySubdomain struct {
	Ref       string
	Name      string
	CreatedAt time.Time
}

// DomainStore is the storage of custom hostnames and vanity subdomains. Both registries
// implement it; it is a separate interface so that code that does not touch domains does not
// need it. Get* return ErrNotFound; writes return ErrConflict on a name another project holds.
type DomainStore interface {
	GetCustomHostname(ctx context.Context, ref string) (*CustomHostname, error)
	// PutCustomHostname starts (or restarts) the claim of h.Hostname by h.Ref, replacing the
	// project's earlier claim. It is refused with ErrConflict while the project's hostname is
	// active (delete it first) or while another project holds h.Hostname.
	PutCustomHostname(ctx context.Context, h *CustomHostname) error
	// UpdateCustomHostname writes the status, the DNS flags and the timestamps of the
	// project's claim, nothing else. ErrNotFound without a claim; ErrConflict when it would
	// take a hostname another project holds, or when the claim was replaced meanwhile (the
	// stored hostname differs from h.Hostname) or is active.
	UpdateCustomHostname(ctx context.Context, h *CustomHostname) error
	// ActivateCustomHostname turns a verified claim (status 4) on in one step: the status
	// becomes 5 and the hostname gets its route. ErrConflict when the claim is not verified or
	// the hostname is routed to another project already; ErrNotFound without a claim.
	ActivateCustomHostname(ctx context.Context, ref string) (*CustomHostname, error)
	// DeleteCustomHostname removes the claim and, when it was active, its route.
	DeleteCustomHostname(ctx context.Context, ref string) error
	ListCustomHostnames(ctx context.Context) ([]CustomHostname, error)
	// ReleaseStaleClaims sets every verified but not active claim on hostname whose last proof
	// (VerifiedAt) is before cutoff back to HostnameInitiated, so that it stops holding the
	// name, and returns how many it released. Active claims are never touched.
	ReleaseStaleClaims(ctx context.Context, hostname string, cutoff time.Time) (int, error)

	GetVanitySubdomain(ctx context.Context, ref string) (*VanitySubdomain, error)
	// VanitySubdomainOwner returns the ref that holds name; ErrNotFound when it is free.
	VanitySubdomainOwner(ctx context.Context, name string) (string, error)
	// PutVanitySubdomain gives ref the vanity subdomain name (replacing its previous one)
	// and routes host to it, in one step. ErrConflict when another project holds the name.
	PutVanitySubdomain(ctx context.Context, ref, name, host string) error
	// DeleteVanitySubdomain removes ref's vanity subdomain and its route.
	DeleteVanitySubdomain(ctx context.Context, ref string) error
}

// Domains returns reg's DomainStore, or nil when reg has none.
func Domains(reg Registry) DomainStore {
	d, _ := reg.(DomainStore)
	return d
}
