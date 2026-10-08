package infra

// Current is the infrastructure revision this release needs. The CloudFormation template that the
// release ships (deploy/cloudformation/supavise.yaml) is at this revision: its output InfraRevision
// and the supavise:infra tag of its instance say so, and a test keeps the three equal. Raise it
// with the template, in the same change that adds a capability below.
const Current = 2

// Capability is something a stack can have that the node depends on.
type Capability struct {
	// Name is the stable name of the capability. A stack lists the ones it has switched on in the
	// instance tag supavise:caps.
	Name string
	// Title is the sentence shown to the operator when the stack lacks it.
	Title string
	// Why says what wants it, as the words that follow the title in the status block.
	Why string
	// Feature is the name of what the capability holds back ("replicas", "S3 Storage").
	Feature string
	// Since is the first revision whose template has it.
	Since int
	// Optional capabilities exist in the template from Since on but are off until the operator turns
	// them on with the stack parameter named in Enable. A stack at revision Since or later is not
	// behind for lack of one.
	Optional bool
	Enable   string
	// Role marks a tag that describes the stack ("replica-server") and is never missing.
	Role bool
}

// capabilities is the table of what each revision added. Revision 1 is the template of v0.1.x:
// no instance tags, no Storage bucket. A line is added here, and Current raised, with every
// template change a node has to know about.
var capabilities = []Capability{
	{Name: "tags", Title: "Instance tags readable from the node", Why: "needed to find the stack from the node",
		Feature: "replicas", Since: 2},
	{Name: "storage-role", Title: "Storage bucket and role", Why: "needed for S3 Storage and server failover",
		Feature: "S3 Storage", Since: 2},
	{Name: "peer-rule", Title: "Peer rule in the security group", Why: "needed to add a second server",
		Feature: "replicas", Since: 2, Optional: true, Enable: "PeerCidr1=<peer address>/32"},
	{Name: "fencing", Title: "Failover permissions", Why: "needed for automatic failover",
		Feature: "failover", Since: 2, Optional: true, Enable: "Failover=on"},
	{Name: "replica-server", Since: 2, Role: true},
}

// Capabilities returns the table, in the order the status block lists it.
func Capabilities() []Capability {
	return append([]Capability(nil), capabilities...)
}

// missingSince lists what a stack at revision have lacks for revision need: the capabilities that
// were added after have, up to need, in table order. Optional ones are included, since a stack at
// the older revision has neither the resource nor the parameter that turns it on.
func missingSince(have, need int) []Capability {
	var out []Capability
	for _, c := range capabilities {
		if !c.Role && c.Since > have && c.Since <= need {
			out = append(out, c)
		}
	}
	return out
}
