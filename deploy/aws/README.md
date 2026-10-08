# deploy/aws

The AWS command line path of Supavise: `deploy.sh` creates a stack, brings an existing one to the template of a release, adds a second server and deletes a stack; `rehearse.sh` tries the update on a throwaway stack first. A release attaches `deploy.sh` as `supavise-aws-deploy.sh`, signed. The template is `deploy/cloudformation/supavise.yaml`.

```
supavise-aws-deploy.sh --region us-east-1 --email you@example.com          # a new stack
supavise-aws-deploy.sh update  --stack supavise                            # bring a stack forward
supavise-aws-deploy.sh status  --stack supavise
supavise-aws-deploy.sh replica --leader-stack supavise --region eu-west-1 --az eu-west-1b --token-file token.txt
supavise-aws-deploy.sh --region us-east-1 --stack-name supavise --delete
```

Every command takes `--dry-run`, which prints the `aws` commands and runs none. The script runs on bash 3.2 and later; `update`, `status` and `replica` also need `python3` (standard library only, no `jq`). Run it in AWS CloudShell, on a laptop with the AWS CLI v2, or on the node with `sudo -E supavise upgrade --aws`, which runs it.

## update

`update` changes a stack to the template of the release the script belongs to, and only in ways that cannot replace or interrupt the node. Steps:

1. It reads the instance id, the region and the tag `supavise:stack-name` from the node's metadata service when it runs on one, then switches the metadata service off for every `aws` call. The stack is changed with your credentials; a credential that is an instance role is refused.
2. It reads the stack (`describe-stacks`) and refuses one that is not at rest (created, updated, or rolled back to its last update); a stack that is changing or whose creation failed has to be waited for or deleted. On a node, the stack's `InstanceId` output must be this instance. A stack made in the console with an empty `AmiId` gets the image its instance runs as `AmiId`, so that the update cannot replace the instance for a newer image.
3. It builds the parameters: `UsePreviousValue` for every parameter that the stack has and the template declares, `NoEcho` ones included (so `KeyEscrowPassphrase` and `AdminEmail` need not be given again), the value of each `--set NAME=VALUE`, and nothing for a parameter that is new to the stack, which takes its default. `SupaviseVersion` cannot be set: it feeds the user data, and a different value would replace the instance.
4. It downloads the template of the release (unless `--template FILE` is given), checks the signature of `SHA256SUMS` with the release key stamped into the script, the template against the list, and the script itself against the list.
5. A template over 51,200 bytes, the size the API takes inline, is copied to `s3://<BackupBucket>/_stack/<sha256>.yaml` and read from there (`--template-bucket` names another bucket in the stack's region).
6. It creates a change set (`create-change-set --change-set-type UPDATE`), prints every resource change and sorts each into allowed, refused or blocked.
7. It asks you to type `apply` (`--yes` skips the question), runs the change set and waits.

| Change | Result |
|---|---|
| a resource is added | allowed |
| an instance, an Elastic IP or a role changes its tags; an instance changes `MetadataOptions`; a role, a policy or a bucket policy changes its policy document; a security group changes its rules; none of it replaces anything | allowed |
| any resource is removed | refused |
| a resource that holds state or identity is replaced (the instance, the volume and its attachment, the buckets, the Elastic IP and its association, the VPC and subnet, the security group, a role, an instance profile, a secret) | refused |
| anything else in place, such as `UserData` or `InstanceType` of the instance | refused |
| the service address association or the Elastic IP would change after a failover moved the address to another server | blocked |
| a rule, record or policy that is not on the list above is replaced | allowed, shown as `replace` |

A refused change set is deleted and nothing changes (exit 2). `--allow-risky` runs it after you type the stack name, on a terminal; a blocked one is never run. Whether the service address is on the stack's instance is read with `describe-addresses`; an address that cannot be read counts as moved.

Exit status of `update`, `status` and `replica`: 0 done or nothing to change, 2 refused (bad arguments, a change that is not allowed, a signature that does not verify), 3 failed. `supavise upgrade --aws` reads these.

Before an instance of the stack is replaced on purpose (a changed image or user data does), set `SupaviseVersion` to the release the node runs: a replacement installs `SupaviseVersion` over the data volume, and the value the stack was made with is older. `status` prints this rule.

## status

`status` prints the stack, its state, its instance, the infrastructure revision (`InfraRevision`, 1 for a stack from before revisions), whether the revision of a template given with `--template` is newer, and whether the service address is on the stack's instance.

## replica

`replica` creates a second Supavise server as a stack from the same template with `JoinLeader` set, in another zone or region, and opens the leader to it. In order:

1. It reads the leader stack's outputs and refuses a leader below infrastructure revision 2 (run `update` first).
2. It checks, with a change set that it reviews and deletes, that the leader stack can take the new rule on port 7443. A refusal stops here, before anything exists.
3. It stores the join token (`--token-file`, the output of `supavise node token`, mode 0600) as a Secrets Manager secret in the new server's region. The AWS CLI reads the file; the token is on no command line. The secret is deleted again if the script stops before the stack exists, and after the server has joined.
4. It creates the new stack (`create-change-set --change-set-type CREATE`) with the leader's backup and objects buckets, its Storage role, its cluster name, the leader's Elastic IP as `PeerCidr1` and `AvailabilityZone`. The new server installs the release, verifies the installer against the signed list, reads the token and joins.
5. As soon as the new stack has its Elastic IP, long before the server has booted, it sets the first free `PeerCidr` of the leader stack to that address (`update`, same review) so that the join can connect.
6. It waits for the new stack to finish.

A template over the inline size goes through a bucket in the stack's region: `--template-bucket` if given, else for the new server the leader's backup bucket when the regions are the same, else `supavise-templates-<account>-<region>`, which the script creates (private, kept when the stack is deleted). The leader's own update always uses the leader's backup bucket.

## A new stack

Without a command word, `deploy.sh` creates a stack with `aws cloudformation deploy`, as in the main README. The template is over the inline size, so the CLI stages it in a bucket: `--template-bucket` if given, else `supavise-templates-<account>-<region>`, made on first use with public access blocked and kept in your account.

## Where the template comes from

`update`, `status` and `replica` use `--template FILE` as it is, or download `supavise.yaml`, `SHA256SUMS` and `SHA256SUMS.sig` from the release named by `--version`, else the release the script was attached to. The release key and the tag are stamped into the attached copy by `deploy/release-assets.sh`; a copy from a checkout has none and refuses to verify (use `--template`). Verification needs an OpenSSL that checks ed25519, which CloudShell and Ubuntu have; on macOS `brew install openssl@3` (the script looks in Homebrew's directories).

`deploy/release-assets.sh` stamps first and signs last: `install.sh`, `supavise.yaml` and `supavise-aws-deploy.sh` are in `SHA256SUMS` with the hashes of the files as attached, and `supavise-release.json` names the stack revision, the template asset and its SHA-256 (`aws`) and the host converge revision of the binary (`host`).

Test hooks, as in `install.sh`: `SUPAVISE_DEPLOY_BASE_URL` replaces `https://github.com/supavise/supavise/releases`, `SUPAVISE_DEPLOY_PUBKEY_B64` the stamped key, `SUPAVISE_IMDS_ENDPOINT` the metadata service address.

## rehearse.sh

`rehearse.sh --region R --email E` makes a throwaway stack from the v0.1.1 template (`deploy/cloudformation/testdata/supavise-v0.1.1.yaml`, the file of the tag), updates it to the template of the checkout with the failover permissions and a peer rule on, checks that the instance has the same id and launch time, is running and had no create or delete event, that the stack is at the new revision and that a second update changes nothing, prints what to check on the node, and deletes the stack. It costs money while it runs. `--purge` also empties and deletes the buckets and the snapshots; `--keep` leaves the stack; `--dry-run` prints the plan.

On the node, by hand (the script prints the commands): `supavise:infra`, `supavise:caps` and the other tags appear in the metadata service without a restart, and `aws ec2 stop-instances --dry-run` and `associate-address --dry-run` with the instance role answer `DryRunOperation`. Adding a second server (`replica`) is the other half of the rehearsal and needs a running leader to issue the token.

## Tests

`go test ./deploy/aws/` runs the script against a stub `aws` that records its calls and answers from files, a fake metadata service and a release directory signed with a throwaway key; nothing reaches AWS. It covers the arguments, the dry runs, create and delete, the parameters of an update (`UsePreviousValue`, `--set`, the immutable version, the console stack's image), the staging of a large template, the reviews of change sets (`testdata/changeset-*.json`: the update of a v0.1.1 stack, replacements, removals, in-place changes that are not known to be safe, the association after a failover), the order of the calls, the instance role refusal, the metadata service, the signature, template and script checks, `status`, `replica` (order, parameters, token handling, early stop) and `rehearse.sh`.

Not verified here: a real change set against a real stack. The review reads the shape of `describe-change-set` output as the API reference documents it; `rehearse.sh` is the check against AWS.
