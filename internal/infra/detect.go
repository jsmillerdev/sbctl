package infra

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/supavise/supavise/internal/awsapi"
	"github.com/supavise/supavise/internal/config"
)

// The instance tags the template writes (deploy/cloudformation/supavise.yaml) and the node reads
// from the instance metadata service. They are plain strings; internal/failover reads the cluster
// and address tags the same way.
const (
	TagCluster     = "supavise:cluster"
	TagStackName   = "supavise:stack-name"
	TagInfra       = "supavise:infra"
	TagAddress     = "supavise:eip"
	TagStorageRole = "supavise:storage-role"
	TagCaps        = "supavise:caps"
	TagLeader      = "supavise:leader"
)

// BootstrapLog is written by the user data of the template at first boot. A node that has it
// was created by a stack, whatever its tags say.
const BootstrapLog = "/var/log/supavise-bootstrap.log"

// Source is where Gap looks. DefaultSource reads this machine; tests give it fakes.
type Source struct {
	// OnEC2 reports whether this host is an EC2 instance. The metadata service is asked only then,
	// so a node anywhere else answers at once instead of waiting for a timeout.
	OnEC2 func() bool
	// Tags returns the instance tags from the metadata service. An instance without tags, or one
	// that does not show them, gives an empty map.
	Tags func(ctx context.Context) (map[string]string, error)
	// StackName is the stack name the node's own configuration records ([aws] stack_name), or "".
	StackName func() string
	// Bootstrapped reports whether the user data of a stack ran on this host (BootstrapLog exists).
	Bootstrapped func() bool
}

// Gap computes the Report for this node: what its stack lacks for this release. A host that is
// not an EC2 instance made by a stack gets the zero Report. An error means the node could not tell
// (the metadata service did not answer); it is not a gap.
func Gap(ctx context.Context) (Report, error) { return Detect(ctx, DefaultSource()) }

// Detect is Gap with the places to look given.
func Detect(ctx context.Context, src Source) (Report, error) {
	if !src.OnEC2() {
		return Report{}, nil
	}
	tags, err := src.Tags(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("infra: read the instance tags: %w", err)
	}
	stack := tags[TagStackName]
	if stack == "" {
		stack = src.StackName()
	}
	have := 1
	if v, ok := tags[TagInfra]; ok {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n < 1 {
			return Report{}, fmt.Errorf("infra: instance tag %s is %q, not a revision number", TagInfra, v)
		}
		have = n
	} else if stack == "" && !src.Bootstrapped() {
		// An EC2 instance that no stack made: installed by hand. Nothing here can be fixed by a
		// stack update, so there is no gap to report.
		return Report{}, nil
	}
	r := Report{Platform: "aws", Stack: stack, Have: have, Need: Current, Enabled: splitCaps(tags[TagCaps])}
	if have >= Current {
		return r, nil
	}
	seen := map[string]bool{}
	for _, c := range missingSince(have, Current) {
		r.Missing = append(r.Missing, Missing{Capability: c.Name, Title: c.Title, Why: c.Why})
		if c.Feature != "" && !seen[c.Feature] {
			seen[c.Feature] = true
			r.Features = append(r.Features, c.Feature)
		}
	}
	r.Fix = "sudo -E supavise upgrade --aws"
	if stack == "" {
		// A stack from before revisions has no tag that names it; the upgrade asks for the name.
		r.Fix += " --stack-name NAME"
	}
	return r, nil
}

func splitCaps(v string) []string {
	var out []string
	for _, c := range strings.Split(v, ",") {
		if c = strings.TrimSpace(c); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// DefaultSource reads this machine: the DMI strings for the platform, the metadata service
// through internal/awsapi for the tags, config.toml and config.d for the stack name.
func DefaultSource() Source {
	return Source{
		OnEC2: func() bool { return OnEC2("/sys") },
		Tags:  metadataTags,
		StackName: func() string {
			cfg, err := config.Load("")
			if err != nil {
				return ""
			}
			return cfg.AWS.StackName
		},
		Bootstrapped: func() bool {
			_, err := os.Stat(BootstrapLog)
			return err == nil
		},
	}
}

// metadataTags reads the instance tags. The tags are not a credential, so the switch that keeps a
// tool from using the instance role (AWS_EC2_METADATA_DISABLED, which `supavise upgrade --aws`
// sets for the stack step) does not apply: it is hidden from this client.
func metadataTags(ctx context.Context) (map[string]string, error) {
	// NoInstanceRole: reading tags takes no credentials, so the role is never asked for.
	c, err := awsapi.New(awsapi.Config{NoInstanceRole: true, Getenv: func(k string) string {
		if k == "AWS_EC2_METADATA_DISABLED" {
			return ""
		}
		return os.Getenv(k)
	}})
	if err != nil {
		return nil, err
	}
	return c.IMDS.Tags(ctx)
}

// OnEC2 reports whether the machine whose sysfs is mounted at sysRoot is an EC2 instance: the
// firmware of Nitro instances names Amazon EC2 as vendor, and the older Xen ones say so in the
// BIOS version or the hypervisor UUID. Every file is readable without privileges.
func OnEC2(sysRoot string) bool {
	for _, f := range []string{"class/dmi/id/sys_vendor", "class/dmi/id/bios_vendor", "class/dmi/id/bios_version", "class/dmi/id/product_version"} {
		b, err := os.ReadFile(filepath.Join(sysRoot, f))
		if err == nil && strings.Contains(strings.ToLower(string(b)), "amazon") {
			return true
		}
	}
	b, err := os.ReadFile(filepath.Join(sysRoot, "hypervisor/uuid"))
	return err == nil && strings.HasPrefix(strings.ToLower(strings.TrimSpace(string(b))), "ec2")
}
