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

A joiner that has no CA yet uses `PinnedTLS`: it accepts the server when the root of the chain the server presents hashes to the fingerprint in the join token and the leaf chains to that root.

## Sessions and streams

One multiplexed session per pair of nodes (`xtaci/smux`, protocol 2, in `mux.go` only). Either side opens streams; a stream starts with one JSON line, the `Header`: `{"t":"fwd","kind":"postgres","ref":"<ref>"}` for raw bytes to a loopback port, `{"t":"rpc"}` for one HTTP/1.1 request and answer. The reader ignores fields it does not know (a node one release ahead may add one), refuses a type or kind it does not know and anything after the JSON object on the line, and takes nothing of the bytes that follow.

The windows come from spike S6: 4 MiB per stream and 32 MiB per session (the receiver's setting governs), keepalive every 10 seconds with a 30 second timeout, below `wal_receiver_timeout`. With smaller windows the lag of a standby grows with the round trip.

**Who dials.** The lower node id dials at once. The higher one waits 3 seconds and dials if there is still no session, so a node that cannot be reached from outside still gets a session, and either side can then open streams. A caller that needs a session (`Dial`, `Call`) connects at once, whichever id it has; callers that ask together share one attempt, and a failed attempt is not repeated for a second. When both sides dial at once, both keep the session the lower id opened and the other ends when its last stream does. The address is the node's `peer_addr` first, then whatever the `AddrResolver` returns (on AWS `cluster.AWSResolver`: the instance's current addresses from EC2).

**Ping.** Each session is pinged every 5 seconds over an rpc stream: the round trip is kept (`RTT`), the answer goes to `Options.OnPing`, and a session whose pings fail three times is closed and dialed again.

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

The port is `LocalPort(cfg, kind, ref, seq)`, the number the forwarder on the other side binds. It refuses a sequence below 1 or above `config.MaxProjectSeq()` and, for the replica kinds, above `config.MaxReplicaSeq()`. A node answers from its own copy of the registry and remembers a project's home for one second.

The request policy for rpc streams (`rpcAllowed`) is by caller state: no certificate reaches the join endpoint; a joining node the ping and the join confirmation; a fenced node the ping, the fence and the rejoin; an active node everything. The endpoints that only the leader may call wrap their handler in `RequireLeader`.

## Forwarders

`Forwarders` keeps the listeners in step with the registry. On this node it binds the canonical Postgres, GoTrue and PostgREST ports of every project homed on another active node; the replica ports of every replica of a project that this node holds no replica of (with several replicas the oldest answers); and, on a node that is not the leader, the shared-service ports. It binds only when the registry says the service is not local, and a port that something already holds is tried again every 2 seconds (the old home still stopping). When the registry moves a service the listener follows or goes; a fenced node has none. `Suspend(ref)` closes the listeners of a project that is about to run here (a replica being promoted starts Postgres on the canonical port) until the returned function is called. `ForwardOne` forwards one port through a `Client`, for a joiner that has no registry yet.

`Pipe` copies both ways and closes both when either direction ends: a forward stream is a request and an answer, and the multiplexed stream has no half close.

## Helpers for handlers

`RespondJSON`, `RespondError` (a `peerapi.Error` with a code), `DecodeBody`, `RequireLeader`, `PingHandler`. A non-2xx answer reaches the caller as `*RemoteError` with the status, message and code; `errors.Is(err, ErrRefused)` is true for the peer server's own refusals. `ResetDefaultMux` empties `DefaultMux`; the daemon calls it before its hooks register endpoints.

## Tests

`go test ./internal/mesh/...` runs two or three nodes in one process over loopback sockets with real TLS: calls both ways, forwarding both ways, the authorization table, revocation, a session when only the higher node can dial, simultaneous dials, strangers and the pin, forwarders following the registry, headers and ports.
