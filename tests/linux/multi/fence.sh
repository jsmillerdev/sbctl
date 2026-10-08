#!/bin/bash
# The fence command of the replication test: [failover] fence_command, run by the survivor's daemon through
# /bin/sh before it promotes anything (internal/failover/command). The environment is the move's: OLD_NODE,
# OLD_NODE_ID, OLD_NODE_ADDR, NEW_NODE, NEW_NODE_ID, NEW_NODE_ADDR, EPOCH and PLANNED.
#
# A fence command exits 0 only when the old node can no longer write. Nothing here stops a machine: the test
# stops the old leader itself (tests/linux/multi/replication.sh, `incus stop --force`) before it asks for the
# failover, as an instance fencer would, and this command confirms it: it exits 0 when the old node does not
# answer on its mesh port and 1 when it does. Every call is recorded in the state directory, where the test
# reads that the daemon called it, with which environment, and how it ended.
log=/var/lib/supavise/fence-test.log
stamp() { date -u +%Y-%m-%dT%H:%M:%SZ; }
echo "$(stamp) fence called: OLD_NODE=$OLD_NODE OLD_NODE_ID=$OLD_NODE_ID OLD_NODE_ADDR=$OLD_NODE_ADDR NEW_NODE=$NEW_NODE EPOCH=$EPOCH PLANNED=$PLANNED" >>"$log"
host=${OLD_NODE_ADDR%:*}
port=${OLD_NODE_ADDR##*:}
if [ -z "$host" ] || [ "$host" = "$OLD_NODE_ADDR" ]; then
  echo "$(stamp) fence refused: no host:port for $OLD_NODE" >>"$log"
  echo "fence: the registry has no address for $OLD_NODE" >&2
  exit 1
fi
if timeout 4 bash -c "exec 3<>/dev/tcp/$host/$port" 2>/dev/null; then
  echo "$(stamp) fence refused: $OLD_NODE still answers on $host:$port" >>"$log"
  echo "fence: $OLD_NODE still answers on $host:$port, so it can still write" >&2
  exit 1
fi
echo "$(stamp) fence done: $OLD_NODE does not answer on $host:$port" >>"$log"
exit 0
