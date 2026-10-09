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

// HomeRouter marks a Plane that sends each call for a project to the node the project is homed on
// (the plane router of internal/placement). An Engine drives a project homed on another node only
// through one: the node's own plane would act on whatever the node holds of that project, which is
// at most a replica.
type HomeRouter interface {
	RoutesByHome()
}

var _ FullPlane = (*PostgresPlane)(nil)
