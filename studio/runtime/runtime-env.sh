#!/bin/sh
# Defaults for the launcher. A set variable, including an explicitly empty value, always wins.
# The heap cap is higher than the slim self-hosted artifact (192): platform mode serves more
# pages and the server preloads their modules.
if [ -z "${NODE_OPTIONS+x}" ]; then
  export NODE_OPTIONS='--max-old-space-size=384 --max-semi-space-size=2'
fi
if [ -z "${NEXT_TELEMETRY_DISABLED+x}" ]; then
  export NEXT_TELEMETRY_DISABLED=1
fi
