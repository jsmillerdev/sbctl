# internal/cluster

A node's knowledge of the cluster it belongs to: who the nodes are, which one leads, at what epoch, and whether this node may act as a primary. The mesh (`internal/mesh`) is the transport; this package is membership: the certificate authority, joining, boot, leader detection, renewal, removal and rejoining.

A node is the leader exactly when the system cluster on that node is not in recovery (invariant I1). Nobody sets a flag: promoting the system cluster makes the node the leader. `contract.go` is what other packages code against (`Membership`, `Snapshot`, `Role`); `Static` and `Solo` implement it for a single server and for tests, `Live` for a node in a cluster.

## The certificate authority

`NewCA(secrets)` derives an Ed25519 key from `Derive("supavise/cluster-ca/v1")` (HKDF-SHA256 expands it if `Derive` returns other than 32 bytes) and signs a CA certificate with a fixed subject, serial 1 and a fixed 20-year validity, so every node that holds the master key computes the same bytes. The SHA-256 of that certificate is the pin in a join token. Rotation is another label and a reissue.

A node certificate is Ed25519, one year, usable as server and client, with the subject alternative name `supavise://node/<id>` and a random serial that is recorded in `nodes.cert_serial`. The key is generated on the node and never leaves it. A promoted node can issue certificates, because the CA derives from a key it holds. The files are in `/etc/supavise/cluster`: `node.key` (0600), `node.crt`, `ca.crt`. A server whose directory holds `node.crt` belongs to a cluster as far as the files can say (`Joined`); a server without it is a single server and nothing here changes how it runs.

`Renewer` replaces a certificate when 30 days are left. A follower sends a request to the leader (`POST /peer/v1/certs/renew`), the leader signs it and records the serial, and the node writes the new certificate to `node.crt` as soon as it arrives: the old file stops working once the peers' copies show the serial, so a daemon that restarts at any point after the request loads the new one. The node starts using the renewed certificate in memory once its own copy of the registry shows the serial and 10 seconds have passed (peers admit a certificate by the serial in their copy, and switching sooner would have some of them refuse the node for a moment), or after 2 minutes when the copy still does not show it, because the old certificate is about to be refused. A renewed certificate that could not be written would lock the node out at its next restart, so the renewer creates and removes a file in `/etc/supavise/cluster` when it starts and before it asks for a certificate (`Store.Writable`). When that fails it asks for nothing: the current certificate keeps working until it expires, the log says so at error level and the alert `certificate_expiring` (key `node_cert_renewal`) is raised until the directory can be written. A write that fails after the leader recorded the serial raises the critical form of the same alert (`ErrNotKept`), the new certificate is used in memory, and the renewer writes the file at its next look (`Store.Resync`) and resolves the alert. The daemon's unit has to allow the writes: `ReadWritePaths=` must name `/etc/supavise/cluster` and, for a retirement, `/etc/supavise/config.d`.

## Joining

```
leader                                            joiner
supavise node token  ->  svj1.<token>
                                                  supavise node join --token-file F
                          <- TLS, root hash = ca_fpr ->      (an impostor fails here)
                          GET  /peer/v1/join         -> nonce
                          POST /peer/v1/join         <- {token_id, proof, csr, name, region, ...}
node row `joining`, certificate, system replica row
                          -> cert, CA, master key, cluster settings, system bootstrap
                                                  writes master.key, cluster/*, config.d/10-cluster.toml, join.json
                                                  seeds the system standby; forwards 127.0.0.1:5433 to the leader
                          <- POST /peer/v1/join/confirm     (the standby streams)
node row `active`
```

**The token** is `svj1.` and base64url of `{v, id, leader, ca_fpr, secret, exp, region?, name?}`. The leader keeps `sha256(secret)` in `join_tokens`. The secret is derived from the master key and the token id (`Derive("supavise/join-token/v1/<id>")`), so the leader can check a keyed proof of it with only the hash stored, and nobody who can read the registry (the replicated database, a backup of it) can forge a proof. The proof is HMAC-SHA256 under the secret over the nonce, the certificate request and the name, each length-prefixed; the secret itself never travels. `region` in the token is the joiner's default; the leader accepts any known region.

**The leader checks**, in this order: it is the leader; the nonce is one it gave, unused and under 2 minutes old (at most 1024 are remembered, and an address with no certificate gets 20 challenges at once and one more every 3 seconds, 429 beyond that); the token exists and the proof is right; the token is neither used nor expired; the name is valid, free, and the one the token admits if it names one; the region and the addresses are well formed; the release name is at most 64 characters of letters, digits and `._+~-`; an AWS identity, if given, has the shape the cloud gives its ids (`i-` and 8 to 17 hex digits, a region, a zone, `eipalloc-...`); the version is within the window (same major, minors at most one apart, same Postgres major in the pins); the certificate request is signed by its Ed25519 key. Then it creates the node as `joining`, signs the certificate, creates the system replica row (`system-rr-<region>-<id6>`, origin `system`), asks for a base backup of the system project when the backup service can (`backup.BaseBackupEnsurer`) and uses the token, once, atomically. A refusal before that point costs the token nothing. A node that does not confirm within an hour of its row going to `joining` is removed and its serial revoked (`ReapJoining`). The hour runs from when the row last entered `joining` (`nodes.joined_at`, which `SetNodeState` restarts), so a fenced node that rejoins gets its own hour.

**The joiner** checks the answer before it writes anything: the CA hashes to the pin, the certificate chains to it, is for its key and names the node the leader says. It never overwrites a different `master.key`. `--master-key-file` and `--key-from-escrow` supply the key instead of receiving it (`WithoutKey`); the key must derive the CA the token pins.

`Join(ctx, JoinOptions)` is the function `supavise install --join-token-file` calls. `JoinOptions.Seed` (a `SeedFunc`) builds the system standby from the base backup the leader names and starts it, with the system cluster's replication password, which the leader sends in the bootstrap (`peerapi.SystemBootstrap.ReplicationPassword`; the joiner has no registry to read it from, and `join.json` is 0600 for that reason); its `primary_conninfo` points at `127.0.0.1:<system port>`, where the join serves a forwarder to the leader until the daemon takes over. When the standby streams (`WaitStreaming`, or `JoinOptions.Streaming` in tests) the joiner confirms. A join that stopped after the certificate was issued continues with `JoinOptions.Resume` (`join.json` records the node id, the leader and the bootstrap). `JoinOptions.CheckInputs` checks what needs no leader (the token has not expired, its secret is whole, a supplied master key derives the CA the token pins), and `Join` runs it before anything changes here. `JoinOptions.Preflight` runs next, while the token is still unspent and nothing has changed; an error stops the join there. It is not run for `Resume`, and it must not look at the data directories: a reset sets the old data aside after it ran, and a resumed or repeated seeding finds its own partial copy. Refusing data that the seeding would overwrite is `Seed`'s job. A server that holds an identity it cannot finish with (the leader reaped the join, the node cannot rejoin, the node was removed while it was down) joins again with `JoinOptions.Reset` (`node join --reset`) and a new token: `Retire` stops what runs and sets the data aside, then the join goes on as a new node. The command checks the token first, lists the data it will set aside and asks for confirmation (`--yes` answers it); on a server that leads its cluster (`LeadsHere`: its system cluster is a primary and the registry names this node) it refuses without `--yes`. The identity of a node whose record says it was removed is deleted without the flag.

## Boot

`DecideBoot` runs before the node opens. A single server returns at once. A node with a fenced record is fenced. Otherwise it waits for the system cluster (a primary on the system port, a standby on the replica port), and asks it `pg_is_in_recovery()`: a standby is a follower. For a primary, `Decide` weighs the evidence: the local registry (the cluster row and this node's row), every active peer that answers a ping within 4 seconds (the chain to the cluster CA authenticates the answer; the serials in the local copy of the registry, which may be old, are not consulted), and the leader marker in the backup store (`backup.EpochMarkerStore`, 5 seconds).

| Evidence | Result |
|---|---|
| this node's row is `fenced` or `left` | fenced |
| a source names another leader at an epoch >= this node's | fenced |
| the registry names another leader and no source names this node at a higher epoch | fenced |
| a source names this node at a higher epoch (a promotion the registry has not caught up with) | leader at that epoch; claims of the old leader at the old epoch are stale |
| nothing reachable, and the local record shows no demotion | leader |

After a promotion the boot decision settles on the epoch the marker names, and `AssumeLeadership` records it in the registry: the cluster row names this node at that epoch and the old leader's row becomes `fenced`, unless the cluster row announces maintenance on that node, which is how a planned switchover says that the old leader stopped on purpose and is to be demoted in place (its row stays `active`; the failover procedure clears the announcement).

A fenced node starts no primary: `serveFenced` (`internal/app`) records `fenced.json` with the peers' addresses, stops the units (`FenceLocal`), raises the critical `fenced` alert and answers 503. `FenceLocal` is also what the cooperative fence endpoint of the failover work can call.

## Live

`Live` is the `Membership` of a node in a cluster. It reads the registry and the recovery state every 2 seconds and publishes a `Snapshot` when it changes. While the system cluster is a primary here, `Leader()` is this node even if the registry still names another. `Changed()` closes when the role differs from the boot role on two polls in a row, or at once when the node is fenced: the daemon then stops with `app.ErrRoleChanged` and systemd starts it in the role the cluster gives it.

`ObserveEpoch` is told what a peer's ping says. A running leader whose system cluster answers as a primary, and that sees another node named as leader at its own epoch or a higher one, fences itself (a leader whose database is stopped writes nothing, which is how the old leader of a planned switchover looks until it is demoted): it writes `fenced.json` (with the peers' addresses, which `node rejoin` needs), calls `OnFenced` (the alert, `FenceLocal`) and the daemon restarts into fenced mode. A follower never fences itself on a peer's word.

## Authority and the peer endpoints

`Authority` is the leader's side: `IssueToken`, `Challenge`, `Join`, `Confirm`, `Renew`, `Rejoin`, `ReapJoining`. Every method fails on a node that is not the leader. `PeerAPI.Register` adds the endpoints to a `mesh.Mux`:

| Endpoint | Caller | Answer |
|---|---|---|
| `GET /peer/v1/ping` | any | `peerapi.Ping` |
| `GET`, `POST /peer/v1/join` | no certificate | challenge; the join |
| `POST /peer/v1/join/confirm` | the joining node | 204, the node is active |
| `POST /peer/v1/rejoin` | a fenced node | how to rebuild the system standby |
| `POST /peer/v1/certs/renew` | an active node | the renewed certificate |
| `GET /peer/v1/config` | a follower | the cluster-scoped settings and their revision (leader only) |
| `GET /peer/v1/certs` | a follower | the certificate store with an ETag, 304 on a match (leader only) |
| `POST /peer/v1/report` | a node | 204; the report goes to `Reports` (leader only) |

`Reports` is the leader's memory of what the nodes last reported (replica steps, lag, receiver state, project health). `Reporter` builds this node's report every 10 seconds from the contributors that other packages add with `Add`, and sends it to the leader, or stores it when this node leads.

## Leaving and rejoining

`RemoveNode` (`supavise node rm`, on the leader, through the registry) refuses the leader and a node that is the home of a project, deletes the system replica row, sets the other replicas of the node `GOING_DOWN` for the replica controller to remove, waits for them to go (or deletes the rows with `--force`) and sets the node `left`. A left node is refused at the next handshake (its sessions end after 10 seconds, long enough for it to see its own row through the standby it follows). The node retires itself when its row reads `left` on two polls in a row (`app.retireWhenRemoved`, `Retire`): it stops what runs, sets the data aside under `data.diverged-0`, writes a `fenced.json` with `removed: true`, deletes its cluster identity and the cluster settings, and stops. The record comes before the deletion so that a node whose files cannot be deleted (the unit may not let the daemon write there) still comes up down; the error names what stayed. A node that was down or cut off when `node rm` ran does not learn that it was removed: its peers refuse it and it stays a stale follower until it is reset on that server (`node join --reset`). The daemon then starts as a node that is down, answering 503 with the reason, until `supavise node join` with a token joins it to a cluster again; the join removes the record. `node rejoin` refuses a removed node.

`Rejoin` (`supavise node rejoin`) is for a fenced node. It asks the leader to take it back (the row goes to `joining`; asking again while the row is `joining` gets the same answer, and the hour keeps running), stops what runs, renames the system project's and every project's `data` directory to `data.diverged-<epoch>` (kept for `[failover] keep_diverged_days`, counted from the move: each directory is stamped when it is renamed, because a rename keeps the time of a fenced primary that stopped days ago), seeds a fresh system standby, waits for it to stream, confirms, and removes `fenced.json`. The node keeps its identity and certificate. A rejoin that stopped on the node runs again with the same command, because the fenced record stays until the end. `--leader` names the current leader by address and expects no node id there (the record may name an older leader); without it the address the record kept when the node was fenced is used. A retirement records the removal first, then sets the data aside, then deletes the identity, so a node whose data cannot be moved still comes up down with the reason. Its replicas are rebuilt by the replica controller once the node is active. The size of the lost tail (`pg_controldata` against the fork) is not recorded.

## The AWS resolver

`AWSResolver` implements `mesh.AddrResolver`: for a peer whose row carries an AWS instance id it asks EC2 (`DescribeInstances`, through `internal/awsapi`) for the instance's current addresses, private first when both nodes are in one region, and keeps the answer for 30 seconds. A node that lost its Elastic IP to a takeover is reached this way. The client is the daemon's own, so the lookup covers peers in this node's region; a peer known to be in another region is dialed on the `peer_addr` of its row. The instance role needs `ec2:DescribeInstances`.

## Files the package reads and writes

| File | Content | Mode |
|---|---|---|
| `/etc/supavise/master.key` | written by a join that received the key | 0600 |
| `/etc/supavise/cluster/node.key`, `node.crt`, `ca.crt`, `join.json` | identity; the state of a join in progress | 0600, 0644, 0644, 0600 |
| `/etc/supavise/config.d/10-cluster.toml` | the leader's cluster-scoped settings | 0600 |
| `/var/lib/supavise/fenced.json` | the record of being fenced, with the peers' addresses | 0644 |
| `/var/lib/supavise/cluster-status.json` | the daemon's live view (sessions, lag), written every 10 seconds for `supavise status` and `supavise node ls` | 0644 |

Files are written through a temporary file and a rename and take the owner of their directory, so a join run as root leaves files the daemon can read. No secret appears on a command line or in a log.

## Tests

`go test ./internal/cluster/` covers the CA (identical bytes on two nodes, HKDF, issuing, requests), tokens and proofs, the version window, `Decide` as a table, `Live` (probe, promotion, demotion, fencing), the join over real TLS between two in-process nodes with forwarding both ways and unauthorized streams refused, every refusal (wrong pin, wrong proof, replayed and expired nonce, used and expired token, names, versions, a master key that is not the cluster's, not the leader), resume, reset (and its inputs checked before the node is retired), preflight, rejoin (repeated, and after a long time as a member), renewal (also with a lagging registry copy, a stop while it waits, and a file that cannot be written), a boot ping against an old registry copy, the retention of diverged data from the move, the detail a caller with no certificate is not given, the shapes of what a joiner claims, the rationing of challenges, removal and a retirement whose files stay, `FenceLocal`, diverged data, the AWS resolver against `awsfake`, reports, the status file, and the certificate snapshot.
