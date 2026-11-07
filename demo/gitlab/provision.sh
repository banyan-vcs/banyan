#!/usr/bin/env bash
# GitLab-CE ↔ banyan walkthrough (docs/19 §8, operator guide docs/20):
# every step below is the
# gitaly protocol surface exercised by real GitLab, not a simulation —
# project create (CreateRepository), HTTP push (workhorse →
# PostReceivePack), and the API read-backs the web UI makes
# (GetTreeEntries / FindCommit / ListCommits / GetBlob via Rails).
#
# Usage:  demo/gitlab/provision.sh          # after compose up -d
# Env:    GITLAB_URL (default http://localhost:8180)
#         GITLAB_ROOT_PASSWORD, GITLAB_CONTAINER (defaults match compose)
set -euo pipefail

GITLAB_URL="${GITLAB_URL:-http://localhost:8180}"
ROOT_PASS="${GITLAB_ROOT_PASSWORD:-2pPzp76xwdyirr4B2Kyh}"
GL_CTR="${GITLAB_CONTAINER:-banyan-gitlab-ce}"
BANYAN_URL="${BANYAN_URL:-http://localhost:8170}"
PROJ="banyan-demo"

say() { printf '\n== %s ==\n' "$*"; }

# ---------------------------------------------------------------- wait --
say "waiting for GitLab to come up (first reconfigure takes minutes)"
deadline=$((SECONDS + 900))
# /-/health is IP-allowlisted (the docker gateway gets a 404); the sign-in
# page is the honest "web is serving" probe.
until curl -sf -o /dev/null "$GITLAB_URL/users/sign_in"; do
  if [ "$SECONDS" -gt "$deadline" ]; then
    echo "GitLab not healthy after 15m; tails:" >&2
    docker exec "$GL_CTR" gitlab-ctl status >&2 || true
    exit 1
  fi
  sleep 10
done
echo "healthy after ${SECONDS}s"

# ------------------------------------------------ post-receive adapter ----
# banyan needs GitLab's gitlab_shell secret to sign internal API calls; it
# lives outside the shared config volume, so publish it there and bounce
# banyan (the adapter config refuses to boot on an unreadable secret).
say "sharing the gitlab_shell secret with banyan (post-receive adapter)"
docker exec "$GL_CTR" cp //var/opt/gitlab/gitlab-rails/etc/gitlab_shell_secret \
  //etc/gitlab/gitlab_shell_secret 2>/dev/null || true
if docker exec banyan-gitlab sh -c '[ ! -f /etc/gitlab-shared/gitlab_shell_secret ]' 2>/dev/null; then
  echo "note: secret not yet visible — adapter disabled this run"
else
  docker restart banyan-gitlab >/dev/null
  sleep 3
  docker logs banyan-gitlab 2>&1 | grep -q "post-receive adapter" \
    && echo "adapter armed" || echo "warning: adapter did not announce itself"
fi

# ------------------------------------------------------------- root PAT --
say "seeding the root account if this is a fresh volume"
users=$(docker exec -u git "$GL_CTR" gitlab-rails runner "print User.count" 2>/dev/null | tail -n1 | tr -dc '0-9')
if [ "${users:-1}" = "0" ]; then
  # the admin seed reads ENV['GITLAB_ROOT_PASSWORD'] (003_admin.rb) — a
  # first boot carries it via compose env; a manual reseed passes it here
  docker exec -e GITLAB_ROOT_PASSWORD="$ROOT_PASS" -u git "$GL_CTR" gitlab-rake db:seed_fu >/dev/null
  echo "seeded"
else
  echo "users present ($users)"
fi

say "minting a root PAT via rails runner"
PAT=$(docker exec -u git "$GL_CTR" gitlab-rails runner '
  u = User.find_by_username("root")
  plain = SecureRandom.hex(20)
  t = u.personal_access_tokens.new(name: "banyan-demo", scopes: ["api"],
                                   expires_at: 30.days.from_now)
  t.set_token(plain) # 19.x: set_token takes the plaintext, stores encrypted
  t.save!
  puts plain
' | tail -n1 | tr -d '\r')
[ -n "$PAT" ] || { echo "no PAT minted"; exit 1; }
echo "PAT: ${PAT:0:6}…"

api() { # api METHOD path [json-body]
  local m=$1 p=$2 b=${3:-}
  if [ -n "$b" ]; then
    curl -sf -X "$m" -H "PRIVATE-TOKEN: $PAT" -H "Content-Type: application/json" \
      -d "$b" "$GITLAB_URL/api/v4$p"
  else
    curl -sf -X "$m" -H "PRIVATE-TOKEN: $PAT" "$GITLAB_URL/api/v4$p"
  fi
}

# --------------------------------------------------------- create repo --
say "creating project $PROJ (Rails → gitaly CreateRepository → banyan)"
create_out=$(curl -s -X POST -H "PRIVATE-TOKEN: $PAT" -H "Content-Type: application/json" \
  -d "{\"name\":\"$PROJ\",\"path\":\"$PROJ\",\"visibility\":\"private\"}" -w "\n%{http_code}" "$GITLAB_URL/api/v4/projects")
create_code=$(tail -n1 <<<"$create_out"); create_body=$(head -n -1 <<<"$create_out")
if [ "$create_code" != "201" ]; then
  # idempotent rerun: fetch the existing project instead
  create_body=$(api GET "/projects/root%2F$PROJ")
  [ -n "$create_body" ] || { echo "create failed: $create_code $create_body" | head -c 400; exit 1; }
  echo "reusing existing project"
fi
proj_id=$(grep -oE '"id":[0-9]+' <<<"$create_body" | head -n1 | grep -oE '[0-9]+')
echo "project id: $proj_id"
PUSH_URL="http://root:$PAT@localhost:8180/root/$PROJ.git"

# ------------------------------------------------------------- push git --
say "pushing commits over HTTP (workhorse → PostReceivePack)"
work=$(mktemp -d)
if git clone -q "$PUSH_URL" "$work" 2>/dev/null; then
  echo "cloned existing repo; appending one commit"
  printf 'walkthrough stamp %s\n' "$(date +%s)" >> "$work/README.md"
  git -C "$work" add -A
  git -C "$work" -c user.name=Demo -c user.email=demo@example.com commit -qm "walkthrough stamp"
else
  echo "fresh project: seeding two commits"
  git -C "$work" init -q -b main .
  printf 'hello from gitlab-ce through banyan\n' > "$work/README.md"
  mkdir -p "$work/src"
  printf 'package main\n\nfunc main() {}\n' > "$work/src/main.go"
  git -C "$work" add -A
  git -C "$work" -c user.name=Demo -c user.email=demo@example.com commit -qm "add readme and src"
fi
git -C "$work" push -q origin main 2>&1 | grep -v "^warning:" || git -C "$work" push -q "$PUSH_URL" main

# ------------------------------------------------------------ read-back --
# Rails caches repository reads (root_ref etc.); the post-receive adapter
# (armed above) expires them through the internal-API chain. The explicit
# expiry below stays as a fallback for runs where the adapter is unarmed.
say "expiring Rails repository caches (fallback; adapter does this live)"
docker exec -u git "$GL_CTR" gitlab-rails runner \
  "Project.find_by_full_path(\"root/\#{ARGV[0]}\").repository.expire_all_method_caches" -- "$PROJ" >/dev/null 2>&1 || true

say "read-backs the web UI makes (Rails → gitaly read RPCs → banyan)"
echo "-- default branch:"; api GET "/projects/$proj_id" | grep -o '"default_branch":"[^"]*"'
echo "-- tree:";           api GET "/projects/$proj_id/repository/tree" | grep -o '"name":"[^"]*"'
echo "-- commits:";        api GET "/projects/$proj_id/repository/commits?per_page=5" | grep -o '"title":"[^"]*"'
echo "-- blob:";           api GET "/projects/$proj_id/repository/files/README.md/raw?ref=main"

say "banyan's own face sees the same repo (@hashed → storage id)"
curl -sf "$BANYAN_URL/api/v1/repos" | grep -oE '"id":"[0-9a-f]{16}[^"]*"' | head -5

say "walkthrough complete — GitLab-CE is running on banyan storage"
