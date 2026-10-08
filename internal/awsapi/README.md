# internal/awsapi

The AWS calls Supavise makes itself, with Signature Version 4 and nothing outside the standard library: EC2 (find a peer, stop it, move an Elastic IP), Secrets Manager (`GetSecretValue`), STS (`AssumeRole`) and the instance metadata service (IMDSv2). `awsfake` is an in-process fake of all four for other packages' tests.

```go
c, err := awsapi.New(awsapi.Config{})          // region and credentials are found at the first call
peer, err := c.EC2.DescribeInstances(ctx, awsapi.DescribeInstancesInput{InstanceIDs: []string{id}})
_, err = c.EC2.StopInstances(ctx, awsapi.StopInstancesInput{InstanceIDs: []string{id}, DryRun: true}) // nil: it would be allowed
secret, err := c.SecretsManager.GetSecretValue(ctx, awsapi.GetSecretValueInput{SecretID: arn})
tags, err := c.IMDS.Tags(ctx)                  // supavise:cluster, supavise:infra, ...
```

## Credentials

`Config.Credentials` is a `CredentialProvider`. Left nil, the client uses `DefaultCredentials`: the standard `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` and `AWS_SESSION_TOKEN` variables first, then the instance role from the metadata service, cached and renewed five minutes before it expires. With the variables set, credentials never come from the metadata service.

| Provider | Gives |
|---|---|
| `EnvCredentials(getenv)` | the standard variables; `ErrNoCredentials` when neither key is set, a plain error when only one is |
| `IMDSCredentials(imds)` | the instance role; every failure wraps `ErrNoCredentials` and its cause |
| `ChainCredentials(...)` | the first provider that has an answer; only `ErrNoCredentials` moves on, and when all do the error wraps each provider's error |
| `CachedCredentials(p, now)` | `p`'s answer until five minutes before it expires; a failed renewal keeps the old credentials while they still work |
| `StaticCredentials(c)` | `c` |
| `(*STS).RoleCredentials(in)` | the credentials of an assumed role, cached; give it to a second client's `Config.Credentials` to call AWS as that role |

`SUPAVISE_AWS_NO_INSTANCE_ROLE=1` (or `Config.NoInstanceRole`) turns off the instance role alone: the default credentials are the `AWS_*` variables and nothing else, and `IMDS.InstanceID`, `Region`, `AvailabilityZone`, `LocalIPv4`, `PublicIPv4` and `Tags` keep working. A tool that must act with the operator's credentials sets it and can still ask which instance it runs on. `AWS_EC2_METADATA_DISABLED=true` is the stronger switch: it turns the metadata service off for every client, the identity reads included. Neither stops a caller from building `IMDSCredentials` itself. A `Credentials` value prints only its access key id, and a `SecretValue` prints only its name.

## Region and endpoints

The region is `Config.Region`, else `AWS_REGION`, else `AWS_DEFAULT_REGION`, else the metadata service. `New` does no network I/O; the first call that needs the region or the credentials looks them up, and a lookup that worked is not repeated.

Each service has a public endpoint (`https://ec2.<region>.amazonaws.com`, `secretsmanager`, `sts`; `amazonaws.com.cn` for `cn-` regions) and an override. An explicit `Config.Endpoints` field wins over the variable, and the variable over the public endpoint. An override must be an `http` or `https` URL.

| Service | Variable | Also read |
|---|---|---|
| EC2 | `AWS_ENDPOINT_URL_EC2` | |
| Secrets Manager | `AWS_ENDPOINT_URL_SECRETSMANAGER` | `AWS_ENDPOINT_URL_SECRETS_MANAGER`, the name the AWS CLI and SDKs use |
| STS | `AWS_ENDPOINT_URL_STS` | |
| Metadata service | `AWS_ENDPOINT_URL_IMDS` | `AWS_EC2_METADATA_SERVICE_ENDPOINT` |

## Calls

| Method | API | Notes |
|---|---|---|
| `EC2.DescribeInstances` | Query API | follows result pages; returns state, private and public IP, zone, every network interface with its private IPs (`Primary`, `PublicIP`) and the tags |
| `EC2.DescribeInstanceStatus` | Query API | `IncludeAll` also lists instances that are not running; system and instance status, scheduled events |
| `EC2.StopInstances` | Query API | `Force`; returns the state change and does not wait, so the caller polls `DescribeInstances` for `stopped` |
| `EC2.AssociateAddress` | Query API | by instance or network interface, optionally one `PrivateIP`; `AllowReassociation` is always sent, `false` or `true`; idempotent across a retry (below) |
| `EC2.DisassociateAddress` | Query API | by association id; idempotent across a retry (below) |
| `EC2.DescribeAddresses` | Query API | allocation id, association id, instance, interface, private IP, tags |
| `SecretsManager.GetSecretValue` | awsJson1.1 | by name or ARN, optional version id or stage; string or binary secrets |
| `STS.AssumeRole` | Query API | role ARN, session name, duration, external id |
| `IMDS.*` | IMDSv2 | `InstanceID`, `Region`, `AvailabilityZone`, `LocalIPv4`, `PublicIPv4`, `Tags`, `RoleCredentials`, `Get` for any path |

Every EC2 call that takes `DryRun` asks only whether the call would be allowed, and returns no results:

| EC2 answer | Result |
|---|---|
| `DryRunOperation` | `nil`: the call would have succeeded |
| `UnauthorizedOperation` | an `*Error` for which `IsAccessDenied` is true: the call would be refused |
| anything else, such as `InvalidInstanceID.NotFound` | that `*Error` |

`AssociateAddress` with `AllowReassociation` false is idempotent across a retry. A send that reached EC2 but lost its answer is sent again and meets `Resource.AlreadyAssociated` for its own work. When the call was sent more than once and `DescribeAddresses` then shows the Elastic IP on the instance or interface it named (and on `PrivateIP`, when it gave one), the call succeeds with the association that exists. An Elastic IP somewhere else, and an `AlreadyAssociated` on the first send, stay errors. Without `PrivateIP` the check is loose: any private address of the named instance counts, and so does any interface of that instance, because `DescribeAddresses` reports the instance for all of them. A caller that cares which address holds the Elastic IP gives `PrivateIP`; the failover code does, and it sends `AllowReassociation` true, which needs no check at all.

`DisassociateAddress` is idempotent the same way: a repeated send that meets `InvalidAssociationID.NotFound` succeeds, because the association is gone. A first send that meets it is an error.

`Instance.State` is EC2's name (`running`, `stopping`, `stopped`, ...). `IMDS.PublicIPv4` returns an empty string for an instance without a public address. `IMDS.Tags` returns an empty map when the instance has no tags or does not expose them in metadata (`InstanceMetadataTags`); a node on a stack without tags reads the same.

## Errors and retries

An API error is an `*Error` with `Service`, `Action`, `StatusCode`, `Code`, `Message` and `RequestID`. `IsCode(err, ...)`, `IsAccessDenied(err)` and `IsNotFound(err)` test it. A request that gets no answer, a 500, 502, 503 or 504, or a throttle or timeout code (`RequestLimitExceeded`, `Throttling`, `ThrottlingException`, `ThrottledException`, `EC2ThrottledException`, `RequestThrottled`, `RequestThrottledException`, `TooManyRequestsException`, `PriorRequestNotComplete`, `RequestTimeout`, whatever the status) is sent again with a new signature, five tries in all (`Config.MaxAttempts`). The waits follow 200 ms, 400 ms, 800 ms and 1.6 s (`Config.RetryBackoff`, doubled each time, 5 s at most), and each is a random point in the upper half of its slot, so the four waits add up to between 1.5 and 3 seconds and clients that failed together do not retry together. A refusal, a 4xx and a `DryRunOperation` are final. A cancelled context ends the wait between tries, and the error then carries the last failure and the context's error.

The worst case for an EC2 endpoint that hangs is five tries of the 30 second HTTP timeout plus the waits, about two and a half minutes, and the client has no deadline of its own. A caller that must not wait that long, such as a fencer or a credential refresher, passes a context with a deadline.

An error names what failed first: `aws <service> <action>:` for a call (also when the credentials or the region could not be found), `aws imds <path>:` for the metadata service and `awsapi:` for the client's own setup. Errors wrap their causes, so `errors.Is` finds `ErrIMDSDisabled`, `ErrIMDSUnreachable` and a cancelled context through the credential chain and the region lookup as well. Compare with `errors.Is`, never with `==`, and do not match error text: `Get` returns `ErrIMDSDisabled` wrapped as `aws imds <path>: ...`, `ChainCredentials` returns an error that matches `ErrNoCredentials` and each provider's error, and the texts have changed (`imds X` became `aws imds X`, `awsapi: ec2 X` became `aws ec2 X`). The default of five tries applies to Secrets Manager and STS calls as well as to EC2.

`errors.Is(err, context.DeadlineExceeded)` is also true when the metadata client's own two second timeout fired, so it does not tell the caller's deadline from the service's silence. A caller that needs to know tests its own `ctx.Err()`, and uses `ErrIMDSUnreachable` for the service.

The metadata service has its own loop: a token request, then the read with `X-aws-ec2-metadata-token`. The token is kept for just under six hours and fetched again after a 401. A refused token request (403, 404, 405) is final: there is no IMDSv1 fallback. A request that gets no answer or a 5xx is tried again, three times in all, on the same jittered schedule. Each request times out after two seconds, so a machine that is not on EC2 and has no credentials in the environment fails after about six seconds with an error for which `errors.Is(err, ErrIMDSUnreachable)` is true. The client then does not ask the service again for 30 seconds: reads, the region lookup and the credentials of a call fail at once with the same error. The memo is looked at before every try, so callers that were waiting behind the first one stop as soon as it has recorded the silence, and a call that the memo refused does not renew it. A caller whose context ended, even part way through its tries, and a service that answered with an error status, are not remembered. The memo has its own lock, so a call that fails at once never waits behind a token request.

## The signer

`Sign(req, body, creds, region, service, now)` sets `X-Amz-Date`, `X-Amz-Security-Token` when the credentials have a token, and `Authorization`. It signs `Host` and every header on the request except `Authorization`, `User-Agent`, `X-Amzn-Trace-Id`, `Expect`, `Connection`, `Transfer-Encoding` and `Content-Length`. The canonical path is the escaped path encoded a second time, as every AWS service except S3 expects; the services here use the path `/`. `internal/backup` does not export a signer (it uses the AWS SDK), so this one is separate. The package only signs. `awsfake.Verify` checks a received request the way an AWS endpoint does, by signing a copy that carries just the headers the request names as signed and comparing; it refuses a request that signs a header `Sign` leaves out, and returns the `awsfake.Scope` (access key id, date, region, service) it was signed for.

## The fake

`awsfake.New(t)` starts one loopback server for all four services and stops it with the test. It checks every signature with `awsfake.Verify`, so a client that signs the wrong thing, or with the wrong credentials, gets `SignatureDoesNotMatch`.

```go
fake := awsfake.New(t)
fake.AddInstance(awsfake.Instance{ID: "i-0self", SecondaryIPs: []string{"10.77.0.11"}})
fake.AddInstance(awsfake.Instance{ID: "i-0peer", StopPolls: 2})
fake.AddAddress(awsfake.Address{AllocationID: "eipalloc-svc", PublicIP: "203.0.113.50", InstanceID: "i-0peer"})
fake.SetEnv(t)                  // code that builds its own client; or fake.Client() / fake.Config()
// ... run the fencer ...
fake.Order()                    // ["ec2:DescribeInstances", "ec2:StopInstances", "ec2:DescribeInstances", ..., "ec2:AssociateAddress"]
```

| Method | Does |
|---|---|
| `SetEnv(t)` | sets the endpoint variables and the region, and clears the credential variables and the two metadata switches, so the client reaches the fake and takes the instance role from its metadata service |
| `Config()`, `Client()` | a configuration or client for the fake that ignores the real environment |
| `Calls()`, `Order(services...)` | every call in arrival order, with its fields, the access key that signed it and the status and code the fake answered with; `Order` gives `"ec2:StopInstances"` or `"ec2:StopInstances(dryrun)"` and leaves out the metadata service unless asked |
| `AddInstance`, `UpdateInstance`, `InstanceState` | instances; `StopPolls` is the number of describe calls that still see `stopping` after a stop, and `StopNeedsForce` keeps an instance at `stopping` until a stop with `Force` |
| `AddAddress`, `AddressOf` | Elastic IPs and their associations |
| `AddSecret`, `AddRole`, `AddCredentials` | secrets, roles that `AssumeRole` may assume, further accepted credentials |
| `SetIMDS`, `IMDS`, `ExpireIMDSTokens` | what the metadata service says (id, region, zone, addresses, tags, role) and its session tokens |
| `Inject`, `Deny` | an API error for an action, for a number of calls or for all; `Applied` makes an EC2 call take effect before it fails, like an answer lost on the way; `Deny` answers the way a role without the permission does, for `DryRun` and real calls alike |
| `SetPageSize` | pages of `DescribeInstances` and `DescribeInstanceStatus` |

What it models: `StopInstances` moves a running instance to `stopping`, then to `stopped` after the chosen number of describe polls; `AssociateAddress` moves an Elastic IP, fails with `Resource.AlreadyAssociated` when it is already associated and `AllowReassociation` is false, releases the Elastic IP that was on the same private address (it stays allocated) and drops an auto-assigned public IP when the primary address gets an Elastic IP; `DryRun` checks the permission and changes nothing; describe calls filter on `instance-id`, `instance-state-name`, `private-ip-address`, `ip-address`, `tag:<key>` (instances) and `allocation-id`, `instance-id`, `public-ip`, `tag:<key>` (addresses). Any other filter name is an error, so a typo in a test fails.

What it does not model: IAM policy evaluation (use `Deny`), tag-scoped resources, a stopped instance releasing anything, clock skew, other actions, other regions and other accounts.

## Tests

`go test ./internal/awsapi/...` (the `test` job of `ci.yml`) covers:

- the signer against the cases of AWS's published Signature Version 4 test suite (`aws-sig-v4-test-suite`, query and path normalization, header ordering and trimming, session tokens, form bodies) and the documented IAM `ListUsers` example, including the derived signing key;
- the exact form body of every EC2, STS and Secrets Manager request, and one signed request's headers;
- `AssociateAddress` and `DisassociateAddress` after a lost answer: success on the named target or with the association gone, an error elsewhere, no check on a first answer;
- replies parsed from XML and JSON fixtures in `testdata/` shaped like the API reference examples;
- the `DryRun` mapping, error parsing, retries (throttle codes, the jittered schedule and its bounds), a cancelled context and endpoint and region resolution;
- the credential chain, the cache, the instance-role path, `AWS_EC2_METADATA_DISABLED` and the role-only switch;
- the metadata service token flow, with an expired token and a refused one, and the memo of a service that does not answer (callers queued behind one that found it silent, a caller that gave up between two tries);
- the fake: the order of the fencing sequence, a stop that needs `Force`, address moves and replacement, `Deny`, faults that take effect, signature checks (`awsfake.Verify` accepts what `Sign` produces and refuses a changed body, query, method, header, host, scope, date, token or secret) and paging;
- that the production package declares no `Verify` or `Scope`.

## Limits

- Only the actions in the table exist. Adding one is a method, a form-field list and a parser.
- A real EC2, STS or Secrets Manager endpoint is never contacted by the tests. The request shapes follow the API reference; whether a role's policy allows `AssociateAddress` with an instance and a private IP is answered by a `DryRun` call on a real stack, which no test here makes.
- Credentials renewal is serialized per client. The memo of an unreachable metadata service belongs to the client that saw the failure.
- `DisassociateAddress` has no caller in the daemon today; its retry rule is here for the first one.
