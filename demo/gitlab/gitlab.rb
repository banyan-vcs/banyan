# GitLab-CE wired to banyan as its Gitaly storage layer (docs/19 §0/§1).
# Mounted into the gitlab-ce container as the omnibus config. The single
# storage entry replaces the embedded gitaly node with banyand's gRPC
# frontend: same token, storage_name "default", address = the compose
# service. Tuned for a 4 GiB-class demo VM (puma/sidekiq trimmed, the
# monitoring stack off) — a production box loosens all of that.
external_url "http://localhost:8180"
gitlab_rails["gitlab_shell_ssh_port"] = 8122

# --- the whole point: git storage goes to banyan -------------------------
# (git_data_dirs was removed in GitLab 18.0; the external-gitaly client
# config moved to gitlab_rails['repositories_storages'])
gitlab_rails["repositories_storages"] = {
  "default" => {
    "gitaly_address" => "tcp://banyan:8075",
    "gitaly_token"   => "banyan-gitlab-demo-token",
  },
}
gitlab_rails["gitaly_token"] = "banyan-gitlab-demo-token"
gitaly["enable"] = false
praefect["enable"] = false

# --- 4 GiB-class footprint -----------------------------------------------
puma["worker_processes"] = 2
sidekiq["max_concurrency"] = 5
prometheus_monitoring["enable"] = false
gitlab_rails["gitlab_default_theme"] = 1

# Deterministic root login for the walkthrough script; the password is
# rotated into a PAT immediately (GITLAB_ROOT_PASSWORD is also read by
# gitlab-ctl during reconfigure).
gitlab_rails["initial_root_password"] = "2pPzp76xwdyirr4B2Kyh"
