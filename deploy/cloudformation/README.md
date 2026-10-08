# deploy/cloudformation

`supavise.yaml` is the one CloudFormation template of Supavise. It makes a node (an instance, an Elastic IP, an encrypted XFS data volume, the buckets, a security group, a narrow instance role) and, with `JoinLeader` set, the replica server of an existing node. A release attaches it as `supavise.yaml` with the release as the default `SupaviseVersion`, signed in `SHA256SUMS`. The deploy path is `deploy/aws/`; the node's view of the stack is `internal/infra`. The section "AWS" of `deploy/README.md` is the operator's guide.

## Infrastructure revision

The template is at a revision (`InfraRevision` output, `supavise:infra` tag of the instance, `infra.Current`). A stack made from an older template is behind; `supavise status` shows the gap and `supavise-aws-deploy.sh update` closes it. A change to the template that a running node must know about raises the revision in the same change as a line in `internal/infra/revisions.go`.

| Revision | What the template has |
|---|---|
| 1 | the template of v0.1.x |
| 2 | instance tags and `InstanceMetadataTags`; `ObjectsBucket` and `StorageRole`; `PeerIngress1..3`; `FencingPolicy`; outputs `ElasticIpAllocationId`, `ObjectsBucket`, `StorageRoleArn`, `SecurityGroupId`, `InfraRevision`, `ClusterName`; the replica server stack |

Every parameter added after revision 1 defaults to off:

| Parameter | Turns on |
|---|---|
| `Failover` (`off`) | `FencingPolicy`: stop and re-address resources that carry the cluster tag, three describe calls |
| `ClusterName` (the stack name) | the value of the `supavise:cluster` tag that the fencing permissions and the Storage role test; give each independent deployment in one account its own (see below) |
| `PeerCidr1..3` | a rule for port 7443 from that range, one `AWS::EC2::SecurityGroupIngress` each |
| `JoinLeader`, `JoinTokenSecretArn`, `BackupBucketName`, `BackupBucketRegion`, `ObjectsBucketName`, `StorageRoleArn` | the replica server stack: no buckets, no claim token, a short user data that installs the verified release and joins, a second private address |
| `AvailabilityZone` | the zone of the subnet the stack makes |

## Storage credentials

Storage cannot use the instance role (the metadata service is denied to it). The stack makes `StorageRole`, scoped to `ObjectsBucket`; the instance role may assume exactly that role (`StorageAssumePolicy`); the daemon assumes it and hands Storage the short-lived credentials. No IAM user, access key or stored secret exists. The role trusts the stack's instance role by ARN and the roles (never users) of the cluster's other servers by their `supavise:cluster` tag.

The tag is what ties the servers of one cluster together, in every region: the fencing permissions cover the instances and addresses of the account that carry it, and the Storage role admits the roles that carry it. Stack names repeat across regions, so two independent deployments in one account that both keep the default (`supavise` in us-east-1 and in eu-west-1, say) share a cluster name, and a server with `Failover` on could stop the other deployment's instance. Set a distinct `ClusterName` for each.

The same tag decides who may reach the objects bucket and the fencing permissions, so `iam:TagRole` on an instance role of the account is equivalent to both: a principal that may tag a role with the cluster name and may assume roles can assume `StorageRole` and take what `FencingPolicy` allows. Grant `iam:TagRole` on instance roles as narrowly as `sts:AssumeRole`.

## What the tests guard

| Test | Guards |
|---|---|
| `TestUpgradeFromV011` (`upgrade_test.go`) | An update of a stack made from `testdata/supavise-v0.1.1.yaml` (the file of the tag) changes nothing that can replace or interrupt the instance, volume, address or network: the two templates are rendered with the parameters of a v0.1.1 stack, conditions applied, and every property of a resource that exists in both is compared; the user data is byte for byte the same; the only new resources are the Storage bucket and role; new parameters have defaults. `TestUpgradeGuardCatchesReplacements` makes sure the comparison fails for the edits it is meant to catch. |
| `TestInstanceRoleIsLeastPrivilege` | Every action the instance role may take. `sts:AssumeRole` only on `StorageRole`; `ec2:` only in `FencingPolicy` and only for tagged resources. |
| `TestStorageRole`, `TestPeerRules`, `TestFencingPolicy`, `TestReplicaServerStack`, `TestInstanceTagsForTheNode` (`replica_test.go`) | the pieces of revision 2 |
| `TestReplicaUserData*` | the replica user data, run with stand-ins for `curl` and the AWS CLI: the installer runs only after the signature, the checksum and the key check, and the token is read after that |
| `TestReleaseAssets*` | what `deploy/release-assets.sh` attaches and signs |

cfn-lint and checkov (`ci.yml`) check the syntax and the generic security rules. No test creates anything in AWS; `deploy/aws/rehearse.sh` is the check against a stack.
