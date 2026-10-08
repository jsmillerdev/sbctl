# deploy/aws

The AWS command line path of Supavise: `deploy.sh` creates a stack, brings an existing one to the template of a release, adds a second server, deletes a stack and destroys what a stack leaves behind; `rehearse.sh` tries the update on a throwaway stack first. A release attaches `deploy.sh` as `supavise-aws-deploy.sh`, signed. The template is `deploy/cloudformation/supavise.yaml`.

```
supavise-aws-deploy.sh --region us-east-1 --email you@example.com          # a new stack
supavise-aws-deploy.sh update  --stack supavise                            # bring a stack forward
supavise-aws-deploy.sh status  --stack supavise
supavise-aws-deploy.sh replica --leader-stack supavise --region eu-west-1 --az eu-west-1b --token-file token.txt
supavise-aws-deploy.sh --region us-east-1 --stack-name supavise --delete
supavise-aws-deploy.sh --region us-east-1 --stack-name supavise --delete --purge   # and leave nothing behind
supavise-aws-deploy.sh purge   --region us-east-1 --stack-name supavise            # for a stack that is already deleted
```

Every command takes `--dry-run`, which prints the `aws` commands and runs none. The script runs on bash 3.2 and later; `update`, `status` and `replica` also need `python3` (standard library only, no `jq`). Run it in AWS CloudShell, on a laptop with the AWS CLI v2, or on the node with `sudo -E supavise upgrade --aws`, which runs it. `purge` and `--delete --purge` need the AWS CLI and nothing else.

## update

`update` changes a stack to the template of the release the script belongs to, and only in ways that cannot replace or interrupt the node. Steps:

1. It reads the instance id, the region and the tag `supavise:stack-name` from the node's metadata service when it runs on one, then switches the metadata service off for every `aws` call. The stack is changed with your credentials; a credential that is an instance role is refused. The region is `--region`, else `AWS_REGION` or `AWS_DEFAULT_REGION`, else the node's. When the region came from the environment (`sudo -E` keeps whatever you exported) and the node's own tag names the stack being updated, the node's region is used and the script says so, because an instance belongs to a stack of its own region; `--region` is taken as it is.
2. It reads the stack (`describe-stacks`) and refuses one that is not at rest (created, updated, or rolled back to its last update); a stack that is changing or whose creation failed has to be waited for or deleted. A stack from before revision 2 has no instance tags to name it, so on a node its `InstanceId` output must be this instance (update it from the node it belongs to or from CloudShell); a stack at revision 2 is matched by the node's `supavise:stack-name` tag, and any other host may update it. A stack made in the console with an empty `AmiId` gets the image its instance runs as `AmiId`, so that the update cannot replace the instance for a newer image.
3. It builds the parameters: `UsePreviousValue` for every parameter that the stack has and the template declares, `NoEcho` ones included (so `KeyEscrowPassphrase` and `AdminEmail` need not be given again), the value of each `--set NAME=VALUE`, and nothing for a parameter that is new to the stack, which takes its default. `SupaviseVersion` cannot be set: it feeds the user data, and a different value would replace the instance.
4. It downloads the template of the release (unless `--template FILE` is given), checks the signature of `SHA256SUMS` with the release key stamped into the script, the template against the list, and the script itself against the list. A list that does not name the script is refused. The script is checked on this path only: with `--template FILE` nothing says which release the script belongs to, so a caller that passes a file has verified the script itself.
5. A template over 51,200 bytes, the size the API takes inline, is uploaded to `s3://<BackupBucket>/_stack/<sha256>.yaml` (`s3api put-object` with your account as the expected bucket owner) and read from there. `--template-bucket` names another bucket of your account in the stack's region. The stack of a replica server shows its leader's bucket as `BackupBucket`; when that bucket is in another region than the stack, CloudFormation could not read a template from it, so the script stages the template in `supavise-templates-<account>-<region>` (made on first use) instead, and the leader's bucket gets no `_stack/` object from a replica's update.
6. It creates a change set (`create-change-set --change-set-type UPDATE`) and reads back the template it holds (`get-template --template-stage Original`): a template that differs from the verified file is refused and the change set deleted (exit 2). The instance role may write to the backup bucket, so the object could be swapped between the upload and CloudFormation's read; the read-back closes that gap. It then prints every resource change and sorts each into allowed, refused or blocked.
7. It asks you to type `apply` (`--yes` skips the question), runs the change set and waits.

| Change | Result |
|---|---|
| a resource is added | allowed |
| an instance, an Elastic IP or a role changes its tags; an instance changes `MetadataOptions`; a role, a policy or a bucket policy changes its policy document; a security group changes its rules; none of it replaces anything | allowed |
| a security group rule or an IAM policy is removed (turning `Failover` off, clearing a `PeerCidr`) | allowed, shown as `remove` |
| any other resource is removed | refused |
| a resource other than a security group rule, an IAM policy or a bucket policy is replaced (the instance, the volume and its attachment, the buckets, the Elastic IP and its association, the network, the DNS records, a role, a secret, and any type this script does not list) | refused |
| anything else in place, such as `UserData` or `InstanceType` of the instance | refused |
| the service address association or the Elastic IP would change, or the instance would be updated in any way, after a failover moved the address to another server | blocked |
| a security group rule, an IAM policy or a bucket policy is replaced | allowed, shown as `replace` |

A refused change set is deleted and nothing changes (exit 2). `--allow-risky` runs it after you type the stack name, on a terminal; a blocked one is never run. Whether the service address is on the stack's instance is read with `describe-addresses`; an address that cannot be read counts as moved. It is read again after the review and the `apply` question, right before the change set runs: a failover in between that makes the change set blocked stops it. The instance is held back too because the Instance resource documents that CloudFormation attaches an Elastic IP again after it updates an instance; whether a change of tags only does so is for `rehearse.sh` to show, so an update of the instance waits for the address to be back (move it back first, then update).

Exit status of `update`, `status`, `replica` and `purge`: 0 done or nothing to change, 2 refused (bad arguments, a change that is not allowed, a signature that does not verify), 3 failed (including a helper that crashes). `supavise upgrade --aws` reads these.

Before an instance of the stack is replaced on purpose (a changed image or user data does), set `SupaviseVersion` to the release the node runs: a replacement installs `SupaviseVersion` over the data volume, and the value the stack was made with is older. `status` prints this rule.

## status

`status` prints the stack, its state, its instance, the infrastructure revision (`InfraRevision`, 1 for a stack from before revisions), whether the revision of a template given with `--template` is newer, and whether the service address is on the stack's instance.

## replica

`replica` creates a second Supavise server as a stack from the same template with `JoinLeader` set, in another zone or region, and opens the leader to it. In order:

1. It reads the leader stack's outputs and refuses a leader below infrastructure revision 2 (run `update` first).
2. It checks, with a change set that it reviews and deletes, that the leader stack can take the new rule on port 7443. A refusal stops here, before anything exists.
3. It stores the join token (`--token-file`, the output of `supavise node token`, mode 0600) as a Secrets Manager secret in the new server's region. The AWS CLI reads the file; the token is on no command line. The secret is deleted again if the script stops before the stack exists, and after the server has joined.
4. It creates the new stack (`create-change-set --change-set-type CREATE`) with the leader's backup and objects buckets, its Storage role, its cluster name, the leader's Elastic IP as `PeerCidr1` and `AvailabilityZone`. The new server installs the release of the template used (the script's own release unless `--version` names another; the template's `SupaviseVersion` default), not the leader's version, so update the leader first when the two differ. It verifies the installer against the signed list, reads the token and joins. Its user data signals the stack only after `install.sh` returns, and the installer's join is synchronous (`supavise node join` returns once the system standby streams and the leader has confirmed the join), so a created stack means a node that is active in `supavise node ls`.
5. As soon as the new stack has its Elastic IP, long before the server has booted, it sets the first free `PeerCidr` of the leader stack to that address (`update`, same review) so that the join can connect.
6. It waits for the new stack to finish.

A template over the inline size goes through a bucket in the stack's region: `--template-bucket` if given, else for the new server the leader's backup bucket when the regions are the same, else `supavise-templates-<account>-<region>`, which the script creates (private, kept when the stack is deleted). The leader's own update always uses the leader's backup bucket. Every bucket the script names must belong to your account, and the one it makes is checked against it before use (a bucket of that name that another account made is not used).

When the script stops after it made something, it says what stays and the commands to remove it: the empty stack of a change set that was not run (`REVIEW_IN_PROGRESS`), or, when the leader could not be opened, the new stack and its join token secret.

## delete and purge

`--delete` stops the instance, deletes the stack and names what stays in the account: the backup bucket, the objects bucket of a stack at revision 2 or later (versioned), and the snapshots of the data volume. The stack of a replica server shows its leader's buckets; they are not listed as kept, because they are not that stack's to empty.

`--delete --purge` also destroys what stays, so that nothing is left to clean by hand. Before anything is stopped it finds and counts it: the stack's buckets (named by its outputs; a bucket whose CloudFormation tags name another stack stops the command, exit 2, and the stack of a replica server has none of its own) with their object versions, delete markers and size, and the snapshots of the data volume, found by the stack's id and by the volume. It lists all of that, adds the final snapshot that CloudFormation takes when the stack deletes the volume, and asks once for the stack name (`--yes` skips the question). Then the instance is stopped, the stack is deleted and, once it is gone, the snapshots are listed again, so that the final one is among them, and everything listed is destroyed.

`purge --region R --stack-name N` does the same for a stack that is already deleted. It refuses while the stack exists (exit 2) and finds only what is provably the stack's:

| What | Found by |
|---|---|
| a bucket | the tags CloudFormation put on it name the stack: `aws:cloudformation:stack-name` is the stack, `aws:cloudformation:stack-id` is a stack of that name in this region and account, `aws:cloudformation:logical-id` is `BackupBucket` or `ObjectsBucket`. The bucket name only narrows the search (it starts with the first sixteen letters of the stack name, lower case), so `supavise-templates-<account>-<region>` and the bucket of another stack are never touched |
| the daily snapshots | the `supavise:stack` tag, which holds the stack's id (the volume carried it, and the snapshot policy copies the volume's tags) |
| the final snapshot | the volume: the script does not rely on a tag (CloudFormation's documentation does not say what it tags this snapshot with), but the snapshot names the volume it was taken from, and the volume is known from the daily snapshots or from the records of the deleted stack, which CloudFormation lists for 90 days (`list-stacks`, `describe-stack-resource` with the stack id) |

A stack that was deleted more than 90 days ago and never made daily snapshots leaves a final snapshot that carries no mark: the script says nothing was found and leaves it, and you delete it by its volume id. Another stack's snapshot, a snapshot without the marks, and a bucket the stack did not make are never listed.

A bucket is emptied with the AWS CLI alone. Incomplete multipart uploads are aborted, then `list-object-versions` and `delete-objects` run in batches of up to 1000 until a list comes back empty, versions first and delete markers after, then the bucket is deleted. The batch is written by the CLI's own `--query`, so a key with any character is passed on as it is. A bucket with Object Lock, or a snapshot that an image uses, is reported and the rest goes on; the script then exits 3 and says to run it again. Every bucket call passes your account as the expected owner. Counting a large bucket reads every version, which takes time.

`--dry-run` prints the exact `aws` commands with the names it cannot know yet in angle brackets. Everything that is destroyed is destroyed for good: there is no undo for a deleted object version or snapshot.

## A new stack

Without a command word, `deploy.sh` creates a stack with `aws cloudformation deploy`, as in the main README. The template is over the inline size, so the CLI stages it in a bucket: `--template-bucket` if given, else `supavise-templates-<account>-<region>`, made on first use with public access blocked and kept in your account.

## Where the template comes from

`update`, `status` and `replica` use `--template FILE` as it is, or download `supavise.yaml`, `SHA256SUMS` and `SHA256SUMS.sig` from the release named by `--version`, else the release the script was attached to. The release key and the tag are stamped into the attached copy by `deploy/release-assets.sh`; a copy from a checkout has none and refuses to verify (use `--template`). Verification needs an OpenSSL that checks ed25519, which CloudShell and Ubuntu have; on macOS `brew install openssl@3` (the script looks in Homebrew's directories).

`deploy/release-assets.sh` stamps first and signs last: `install.sh`, `supavise.yaml` and `supavise-aws-deploy.sh` are in `SHA256SUMS` with the hashes of the files as attached, and `supavise-release.json` names the stack revision, the template asset and its SHA-256 (`aws`) and the host converge revision of the binary (`host`).

Test hooks, as in `install.sh`: `SUPAVISE_DEPLOY_BASE_URL` replaces `https://github.com/supavise/supavise/releases`, `SUPAVISE_DEPLOY_PUBKEY_B64` the stamped key, `SUPAVISE_IMDS_ENDPOINT` the metadata service address. They are for tests: the first two replace the trust root of the script, so never set them in a session that changes a real stack.

## rehearse.sh

`rehearse.sh --region R --email E` makes a throwaway stack from the v0.1.1 template (`deploy/cloudformation/testdata/supavise-v0.1.1.yaml`, the file of the tag), updates it to the template of the checkout with the failover permissions and a peer rule on, checks that the instance has the same id and launch time, is running and had no create or delete event, that the stack is at the new revision and that a second update changes nothing, prints what to check on the node, and deletes the stack. It tells `deploy.sh` not to read the metadata service, so that it can run on an EC2 host. It costs money while it runs. `--purge` makes the deletion `deploy.sh --delete --purge`, which empties and deletes both buckets (every object version) and destroys the snapshots of the data volume, so the cleanup is tried against a real account, JMESPath included, on a stack that holds nothing of yours; `--keep` leaves the stack; `--dry-run` prints the plan.

On the node, by hand (the script prints the commands): `supavise:infra`, `supavise:caps` and the other tags appear in the metadata service without a restart, and `aws ec2 stop-instances --dry-run` and `associate-address --dry-run` with the instance role answer `DryRunOperation`, also when the call names a private address of the instance (`--private-ip-address`): a replica server takes the service address on a second private address, and `FencingPolicy` lists no network interface, so an `UnauthorizedOperation` there means a failover onto a replica server could not move the address. Two more checks need the node, because the script cannot see them: the boot time from `uptime -s` is earlier than the start of the update (a reboot keeps the launch time), and an update that changes only the tags of the instance leaves the service address where it was. For the second, move the address to another instance by hand and run `update` with a copy of the template that adds a tag: it is blocked (that is the guard), so make the same change with `aws cloudformation deploy` and read where the address sits. If it went back to the instance, an update after a failover would take the address back and the guard stays; if not, the guard can be narrowed to the properties that do. Adding a second server (`replica`) is the other half of the rehearsal and needs a running leader to issue the token.

## Tests

`go test ./deploy/aws/` runs the script against a stub `aws` that records its calls and answers from files, a fake metadata service and a release directory signed with a throwaway key; nothing reaches AWS. It covers the arguments, the dry runs, create and delete, the parameters of an update (`UsePreviousValue`, `--set`, the immutable version, the console stack's image), the staging of a large template, the reviews of change sets (`testdata/changeset-*.json`: the update of a v0.1.1 stack, replacements, removals, in-place changes that are not known to be safe, the association after a failover), the order of the calls, the instance role refusal, the metadata service, the signature, template and script checks, `status`, `replica` (order, parameters, token handling, early stop) and `rehearse.sh`. The purge is tested against `testdata/fake-aws-purge.py`, a stand-in `aws` that keeps a stack, buckets with versioned objects and snapshots in a JSON file and answers the exact `--query` strings the script sends: the marks that make a bucket or a snapshot the stack's (and the look-alikes that are not), batches of at most 1000, the order of uploads, objects and bucket, the list of snapshots read again after the stack is deleted, the leader's buckets of a replica server, a protected object and an image-held snapshot that do not stop the rest, and the confirmation.

Not verified here: a real change set against a real stack. The review reads the shape of `describe-change-set` output as the API reference documents it; `rehearse.sh` is the check against AWS. Neither is the purge's JMESPath: the fake answers the query strings the script sends and does not evaluate them, so that the CLI accepts them, and that CloudFormation tags S3 buckets as the table says, are for a run against a throwaway account.
