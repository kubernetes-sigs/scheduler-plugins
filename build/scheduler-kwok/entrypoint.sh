# entrypoint.sh
#!/usr/bin/env bash
set -euo pipefail

if [[ "${1:-}" == "version" ]]; then
  # Accept KWOK's probe shape
  exec /bin/kube-scheduler --version
fi

# Default flags; add others as you like (verbosity, bind addr, etc.)
exec /bin/kube-scheduler \
  --config=/etc/kubernetes/sched-cc.yaml \
  --bind-address=0.0.0.0 \
  "$@"
