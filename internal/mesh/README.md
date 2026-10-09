# internal/mesh

The transport between the nodes of a cluster. Every loopback port a consumer expects is either the real service or a listener that forwards to the node that runs it, so the proxy, Supavisor, Realtime, Storage, Functions, pg-meta and the backups keep dialing `127.0.0.1:<port>` whatever node a project lives on (invariant I3).

```go
m := mesh.New(mesh.Options{Topology: membership, Creds: store.Creds, Authz: &mesh.Authorizer{...}})
go m.Serve(ctx, listener)           // the peer port, [node] peer_listen
go m.Run(ctx)                       // sessions to every active node, upkeep
conn, err := mesh.Forward(ctx, m, "n2", mesh.KindPostgres, ref)   // a raw stream to a loopback port on n2
err = m.Call(ctx, "n2", "GET", peerapi.PathPing, nil, &ping)      // one peer API call
mesh.Handle("POST /peer/v1/fence", fn)                            // another package adds its endpoint
```

`contract.go` is what other packages code against: `Kind`, `Header`, `Dialer`, `RPC`, `Mesh`, `Peer`, `HandlerFunc`, `Mux` and `Handle`. The peer API's paths and bodies are in `peerapi`.

## Port and certificates

One TCP port, TLS 1.3, ALPN `supavise-mesh/1`; a handshake that does not negotiate that protocol is refused by either side. A node presents a certificate whose subject alternative name is `supavise://node/<id>`, signed by the cluster CA (derived from the master key, `internal/cluster`). A handshake succeeds when the chain verifies to the CA for the key usage, the certificate names a node, and the registry has a row for it whose serial is the certificate's and whose state is `joining`, `active` or `fenced`. A `left` node, an unknown node and a certificate that is not the recorded one are refused at the handshake; a session to a node the registry stops admitting is closed after 10 seconds. The server asks for a client certificate and verifies one that is given: a client with none is a joiner, and the request policy lets it reach `GET|POST /peer/v1/join` and nothing else.

A caller with no certificate is held to what a joiner needs, because anyone who can reach the port can be one. Its request body is at most 64 KiB and has 10 seconds to arrive (read in full before the handler runs, then the deadline is lifted, so a join that takes long is not cut off); its session has 1 MiB of buffer and 256 KiB per stream instead of 32 MiB and 4 MiB; at most 16 of its streams are open at a time and the rest are closed unread; at most 32 such sessions are open at once, 4 of them from one address; a session that opens no stream within 10 seconds is closed, one that is 2 minutes old is closed once it has no stream, and every one after 15 minutes. Any caller, with a certificate or without, has at most 16 handshakes in flight from one address, because a connection that never finishes its handshake holds its place for 10 seconds. The join endpoint also rations challenges per address (`cluster.Authority.ChallengeFrom`). The limits are `Options.AnonReadTimeout`, `AnonLife`, `AnonFirstStream`, `MaxAnonSessions`, `MaxAnonPerAddr` and `MaxShakesPerIP`; a node that is admitted is not held to any of the first five.

A joiner that has no CA yet uses `PinnedTLS`: it accepts the server when the root of the chain the server presents hashes to the fingerprint in the join token and the leaf chains to that root.

## Sessions and streams

One multiplexed session per pair of nodes (`xtaci/smux`, protocol 2, in `mux.go` only). Either side opens streams; a stream starts with one JSON line, the `Header`: `{"t":"fwd","kind":"postgres","ref":"<ref>"}` for raw bytes to a loopback port, `{"t":"rpc"}` for one HTTP/1.1 request and answer. The reader ignores fields it does not know (a node one release ahead may add one), refuses a type or kind it does not know and anything after the JSON object on the line, and takes nothing of the bytes that follow.

The windows come from spike S6: 4 MiB per stream and 32 MiB per session (the receiver's setting governs), keepalive every 10 seconds with a 30 second timeout, below `wal_receiver_timeout`. With smaller windows the lag of a standby grows with the round trip.

**Who dials.** The lower node id dials at once. The higher one waits 3 seconds and dials if there is still no session, so a node that cannot be reached from outside still gets a session, and either side can then open streams. A caller that needs a session (`Dial`, `Call`) connects at once, whichever id it has; callers that ask together share one attempt, and a failed attempt is not repeated for a second. When both sides dial at once, both keep the session the lower id opened and the other ends when its last stream does. The address is the node's `peer_addr` first, then whatever the `AddrResolver` returns (on AWS `cluster.AWSResolver`: the instance's current addresses from EC2).

**Ping.** The session of an active node is pinged every 5 seconds over an rpc stream: the round trip is kept (`RTT`), the answer goes to `Options.OnPing`, and a session whose pings fail three times is closed and dialed again. A node that is not active is not pinged. Its session belongs to `supavise node join` or `node rejoin`, a `Client` with no peer API to answer with, and counting its silence would close the session that carries the new standby's stream a few seconds into the join. The state is read at every round, so the first ping follows the join confirmation.

**A short call beside the daemon.** A tool of a node that runs next to its daemon and asks one question (`supavise system converge` fetching the cluster settings, `cluster.ConfigSync`) dials with `OneShot(ClientTLS(...))`, which puts a marker in the handshake's server name (`oneshot.supavise.invalid`). A peer that knows the marker serves the session and keeps it out of the table: no pings, no tie-break, and the session the node's daemon holds stays. Without it the tool's session would replace the daemon's (the same side dialed again, and the forward streams on the old one end after a minute) or lose the tie-break and be closed before the call. A node may hold 4 such sessions at once, and each is closed after 2 minutes. A peer from an earlier release ignores the marker and treats the session as it did before, so the call still works against it.

**A session that ended.** When the far end closes a session, smux says so only to the loop that accepts streams, and the manager closes the session there, so the table forgets it at once. Without that, a node that restarted could be turned away for up to the keepalive timeout by the dead session it had held, because the tie-break prefers the session the lower id dialed.

## The authorization table

`Authorizer.Resolve` turns a forward stream into a loopback port or refuses it; a refused stream is closed before any byte, and the reason goes to the log of the node that refused. There is no open relay: the stream names a kind, never a port.

| Caller | May open |
|---|---|
| no certificate | nothing |
| `fenced` | nothing |
| `joining` | `postgres` of `system`, and nothing else |
| `active` | what the rows below allow |

| Kind | The target must |
|---|---|
| `postgres`, `gotrue`, `postgrest` of `<ref>` | know `<ref>` and be its home (`projects.node_id`) |
| `replica-postgres`, `replica-postgrest` of `<ref>` | hold a replica row of `<ref>` |
| `svc:*` | be the leader |

The port is `LocalPort(cfg, kind, ref, seq)`, the number the forwarder on the other side binds. It refuses a sequence below 1 or above `config.MaxProjectSeq()` and, for the replica kinds, above `config.MaxReplicaSeq()`. A node answers from its own copy of the registry and remembers a project's home for one second; the table of what it remembers is swept of expired entries as it grows.

The request policy for rpc streams (`rpcAllowed`) is by caller state: no certificate reaches the join endpoint; a joining node the ping, the join confirmation and the rejoin (a rejoin that stopped on the node asks again); a fenced node the ping, the fence and the rejoin; an active node everything. An endpoint that only the leader may call checks the caller in its handler (`RequireLeader` is the shared check).

## Forwarders

`Forwarders` keeps the listeners in step with the registry. On this node it binds the canonical Postgres, GoTrue and PostgREST ports of every project homed on another active node; the replica ports of every replica of a project that this node holds no replica of (with several replicas the oldest answers); and, on a node that is not the leader, the shared-service ports. It binds only when the registry says the service is not local, and a port that something already holds is tried again every 2 seconds (the old home still stopping). It reads the registry again when the change stream names a change, when the leader or the set of active nodes changes (looked at every 2 seconds, in memory), while a port is unbound or a suspension is on (every 2 seconds), and every 30 seconds as a safety look; without a change stream it reads every 2 seconds. When the registry moves a service the listener follows or goes; a fenced node has none. `Suspend(ref)` closes the listeners of a project that is about to run here (a replica being promoted starts Postgres on the canonical port) until the returned function is called. `ForwardOne` forwards one port through a `Client`, for a joiner that has no registry yet. A `Client` opens a new session to its address when the one it had has ended, so a leader that restarts while a joiner seeds does not end the joiner's forwarding.

A listener whose `Accept` fails with an error about resources (no file descriptors, a connection reset before it was accepted) is waited out with a short back-off, here and in the peer server (`Serve`). Any other error ends it: the peer server returns it, and a forwarder is forgotten so that the next look binds the port again, because a listener nobody accepts on would hold connections for ever.

`Pipe` copies both ways and closes both when either direction ends: a forward stream is a request and an answer, and the multiplexed stream has no half close.

## Helpers for handlers

`RespondJSON`, `RespondError` (a `peerapi.Error` with a code), `DecodeBody`, `RequireLeader`, `PingHandler`. The path of a call may end in a query string, which the handler reads from `r.URL.Query()`. A non-2xx answer reaches the caller as `*RemoteError` with the status, message and code, a 304 included (`Status` 304 and nothing in the answer), so a conditional GET passes its ETag in the query and treats that error as "not modified"; `errors.Is(err, ErrRefused)` is true for the peer server's own refusals. `ResetDefaultMux` empties `DefaultMux`; the daemon calls it before its hooks register endpoints.

## Tests

`go test ./internal/mesh/...` runs two or three nodes in one process over loopback sockets with real TLS: calls both ways, forwarding both ways, the authorization table, revocation, a session when only the higher node can dial, simultaneous dials, strangers and the pin, the limits on callers with no certificate (body size, a body that never arrives, streams, sessions in all and per address, an idle session, a session that opens no stream, handshakes in flight per address), accept errors, a forwarder whose listener died, a client whose session ended, a node's short call that leaves the session of its daemon alone, is limited per node and ends when it stays open, a client that is closed while it dials, a joining node that is not pinged and whose session stays open while its stream runs, a session the far end closed, a fenced node that still hears the ping and nothing else, the query string and the 304 of a call, forwarders following the registry, headers and ports.
