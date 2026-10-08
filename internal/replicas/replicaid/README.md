# internal/replicas/replicaid

Names replicas and writes their rows: `Region` (a node's region, the default region when it names none), `Short` (the six random characters that end an identifier), `Identifier`, `Create` (a row for a project on a node, drawing again when the identifier is taken) and `EnsureSystem`, which records the standby of the system cluster on a joining node (design 2.3) and returns the row it made earlier when called again.

The package imports only `internal/registry` and `internal/config`. That is why it is separate from `internal/replicas`: the replica controller depends on `internal/placement`, which depends on `internal/cluster`, so the cluster join cannot import the controller. The join calls `EnsureSystem`; the controller names every other replica with the same functions, so a replica is named and placed by one rule wherever it is made.
