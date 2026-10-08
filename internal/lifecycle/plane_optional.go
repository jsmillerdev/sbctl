package lifecycle

// FullPlane is a Plane with every optional capability the Engine looks for by type assertion:
// reconfiguring one service, applying Postgres settings, resetting role passwords, recovering a
// cluster that cannot start, checking a render, inspecting extensions, restarting what the daemon
// held back. *PostgresPlane is one. A plane that wraps another (the router in internal/placement
// puts one in front of the node's own plane) must be one too, or the Engine finds each capability
// missing and quietly stops offering it; this interface is what the wrapper is compiled against.
// An Engine feature that starts asserting a new interface adds it here.
type FullPlane interface {
	Plane
	ConfigPlane
	RecoverPlane
	RenderChecker
	RolePasswordPlane
	ExtensionInspector
	PendingRestarter
}

// The capabilities of a FullPlane that the Engine's files define for themselves.
type (
	// ConfigPlane is what Engine.ApplyConfig needs beyond Plane.
	ConfigPlane = configPlane
	// RecoverPlane is what ApplyConfig needs to honor ApplyOptions.Recover.
	RecoverPlane = recoverPlane
	// RenderChecker renders a service's units from the saved settings without starting anything.
	RenderChecker = renderChecker
	// RolePasswordPlane gives a restored cluster the role passwords the registry holds.
	RolePasswordPlane = rolePasswordPlane
)

var _ FullPlane = (*PostgresPlane)(nil)
