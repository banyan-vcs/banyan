#!/usr/bin/env bash
# banyan cluster demo (docs/11) — one command walkthrough:
# scattered multi-replica placement, node failure with continuous reads and
# writes, automatic replica recovery, dynamic per-repo replica adjustment.
#
# Requirements: docker (compose v2), git, curl. Run from the repo root:
#   bash demo/cluster-demo.sh
#
# NOTE: not exercised on a machine without a docker daemon; the same flows
# are pinned by integration tests T23-T26 (go test ./integration/ -run TestHA).
set -euo pipefail
cd "$(dirname "$0")/.."

COMPOSE="docker compose -f docker-compose.cluster.yml"
GW=http://localhost:8100
REFS=http://localhost:8102

say() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
wait_up() {
  for _ in $(seq 1 60); do
    if curl -sf "$1" >/dev/null 2>&1; then return 0; fi
    sleep 1
  done
  echo "timeout waiting for $1" >&2; return 1
}

say "1/7 build & start the cluster (refs + 4 storage nodes + gateway)"
$COMPOSE up -d --build
wait_up "$GW/"
say "cluster status"
curl -s "$GW/api/v1/cluster/nodes"; echo

say "2/7 push a repo (auto-created; every pack lands on 3 of the 4 nodes)"
WORK=$(mktemp -d)
git -c init.defaultBranch=main -c user.name=demo -c user.email=demo@banyan.dev \
  clone -q "$GW/r/demo.git" "$WORK/demo" 2>/dev/null || \
  git -c init.defaultBranch=main init -q "$WORK/demo"
cd "$WORK/demo"
git config user.name demo; git config user.email demo@banyan.dev
git config core.autocrlf false
mkdir -p lib/nested
echo "readme v1" > README.md
echo "a v1" > lib/a.txt
echo "base" > lib/nested/SHARED.txt
git add -A && git commit -qm "demo: initial"
git remote add banyan "$GW/r/demo.git" 2>/dev/null || true
git push -q banyan main
echo "pushed."

say "3/7 clone the root repo and any subtree (ordinary git, no idea it's a cluster)"
git -c core.autocrlf=false clone -q "$GW/r/demo.git" "$WORK/clone"
git -c core.autocrlf=false clone -q "$GW/r/demo.git/tree/lib" "$WORK/lib"
echo "root clone:   $(cat "$WORK/clone/README.md")"
echo "subtree clone: $(cat "$WORK/lib/a.txt") (workspace IS the subtree)"

say "4/7 kill one storage node — reads and writes keep going (R=3, W=2)"
docker stop banyan-s4 >/dev/null
echo "sleeping past the dead threshold (30s in this demo)..."
sleep 35
curl -s "$GW/api/v1/cluster/nodes" | grep -o '"id":"[^"]*","[^"]*","state":"[^"]*"' || true
echo "node list above should show s4 dead."
echo "a v2" > lib/a.txt
git add -A && git commit -qm "demo: written during s4 failure"
git push -q banyan main
git -c core.autocrlf=false clone -q "$GW/r/demo.git" "$WORK/during"
echo "clone during failure sees: $(cat "$WORK/during/lib/a.txt")"

say "5/7 reconciler restores the 3-replica target for the packs s4 held"
curl -s -XPOST "$REFS/internal/reconcile"; echo
echo "(or wait for the background loop; then verify with the next push+clone)"

say "6/7 bring s4 back — it re-registers via heartbeat"
docker start banyan-s4 >/dev/null
sleep 5
curl -s "$GW/api/v1/cluster/nodes" | grep -o '"id":"s4"[^}]*' || true

say "7/7 dynamic per-repo replica adjustment (3 → 2)"
curl -s -XPUT -H 'Content-Type: application/json' -d '{"replicas":2}' \
  "$GW/api/v1/repos/demo/placement"; echo
curl -s -XPOST "$REFS/internal/reconcile"; echo
echo "demo repo now targets 2 replicas; raise it back with replicas:3 any time."

say "teardown: $COMPOSE down -v   (volumes hold all cluster state)"
