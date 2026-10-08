# internal/infra

What the cloud stack of a node lacks for the release the node runs. On AWS a node was made by a CloudFormation stack, and each release needs the stack at some infrastructure revision. `Gap` compares the two and says what is missing and which command closes the gap. The node learns its revision from its own instance tags, through the instance metadata service, so it needs no IAM permission and makes no call to AWS.

```go
r, err := infra.Gap(ctx)     // the zero Report off AWS; an error when the node could not tell
if r.Behind() {
    r.Render(os.Stdout)        // the block of `supavise status`
}
if r.Has("fencing") { ... }    // the stack has switched a capability on
```

```
Infrastructure  AWS stack "supavise" is at revision 1; this release needs 2 for: replicas, S3 Storage, failover
  missing  Instance tags readable from the node   (needed to find the stack from the node)
  missing  Storage bucket and role                (needed for S3 Storage and server failover)
  missing  Peer rule in the security group        (needed to add a second server)
  missing  Failover permissions                   (needed for automatic failover)
  Fix: sudo -E supavise upgrade --aws
```

A report that is not behind writes nothing. The fix names the stack (`--stack-name NAME`) only when the node cannot tell it: a stack from before revisions has no tag that names it, and `[aws] stack_name` in `config.d/20-aws.toml` is empty until `supavise upgrade --aws` has run once.

## Revisions

`Current` is the revision this release needs. The template that the release ships, `deploy/cloudformation/supavise.yaml`, is at that revision: its output `InfraRevision` and the `supavise:infra` tag of its instance say so, a test keeps the three equal, and `deploy/releasetool` refuses a template at another revision when it writes the manifest.

| Revision | Stack | Capabilities it adds |
|---|---|---|
| 1 | the template of v0.1.x: no instance tags, no Storage bucket | none |
| 2 | instance tags, the objects bucket and Storage role, mesh port rules, failover permissions, the replica server stack | `tags`, `storage-role`, `peer-rule` (optional), `fencing` (optional), `replica-server` (a marker) |

`Capabilities()` is the table in `revisions.go`. A capability marked optional exists in the template from its revision on and is off until the operator turns it on with a stack parameter (`PeerCidr1`, `Failover=on`); a stack at that revision is not behind for lack of it. The instance tag `supavise:caps` lists what the stack has switched on, and `Report.Enabled` and `Report.Has` carry the list to the code that needs one: automatic failover asks for `fencing`, Storage on S3 asks for `storage-role`.

## What the node reads

| Tag | Holds |
|---|---|
| `supavise:infra` | the stack's revision; a stack without it counts as revision 1 |
| `supavise:stack-name` | the CloudFormation stack's name |
| `supavise:cluster` | the cluster name the failover permissions test |
| `supavise:eip` | the allocation id of the Elastic IP, the service address |
| `supavise:storage-role` | the ARN of the Storage role |
| `supavise:caps` | the capabilities switched on, comma separated |
| `supavise:leader` | on a replica server, the address of the leader it joined |

The constants are `Tag*` in `detect.go`. `awsapi.IMDS.Tags` reads them. `supavise upgrade --aws` sets `AWS_EC2_METADATA_DISABLED` for the stack step so that the instance role is never used; that switch hides the metadata service from the credential chain only, and the tags are not a credential, so `Gap` reads them with the switch masked.

## When there is no gap to report

`Detect` returns the zero `Report` (`Platform` empty, not behind) when:

- the host is not an EC2 instance: it reads the firmware strings under `/sys/class/dmi/id` (`Amazon EC2` on Nitro, `amazon` in the BIOS version on Xen) and does not ask the metadata service at all, so a node elsewhere answers at once instead of waiting for a timeout;
- the host is an EC2 instance that no stack made: no `supavise:infra` tag, no stack name in the configuration, and no `/var/log/supavise-bootstrap.log`, which the user data of the template writes. Nothing a stack update could fix.

It returns an error, not a gap, when the metadata service does not answer or a `supavise:infra` tag is not a number.

## Tests

`go test ./internal/infra/` covers the report for a stack before revisions, at revision 2 and newer than the release; the hand-installed host and the host off EC2; the tags read through `internal/awsapi` against its fake, with and without `AWS_EC2_METADATA_DISABLED`, and an instance that hides its tags; the firmware strings; the capability table; and the rendering. `deploy/cloudformation` tests check that the names the template writes are the ones this package reads.

## Limits

No test here reads a real instance. That the metadata service shows the tags the template writes, without a restart, is checked on a throwaway stack by `deploy/aws/rehearse.sh` (see `deploy/aws/README.md`).
