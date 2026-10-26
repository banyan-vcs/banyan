package storage

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/banyan-vcs/banyan/internal/model"
)

// Internal wire protocol between cluster roles (docs/10 §2). All JSON over
// HTTP; pack bytes are raw bodies on the storage path. Error convention: a
// non-200 response carries {"error":{"code","message"}} and the client maps
// codes back onto the package's sentinel errors so callers keep using
// errors.Is (CAS semantics, service.Translate, gitwire guards).

// NodeInfo is one storage node as seen through the refs health table.
type NodeInfo struct {
	ID    string `json:"id"`
	Addr  string `json:"addr"` // host:port of the node's internal listener
	State string `json:"state"`
}

// RefsClient talks to the refs service (meta RPC + health).
type RefsClient struct {
	BaseURL string
	HTTP    *http.Client
}

func NewRefsClient(base string) *RefsClient {
	return &RefsClient{BaseURL: strings.TrimSuffix(base, "/"), HTTP: &http.Client{Timeout: 30 * time.Second}}
}

type wireError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func mapCode(code, message string) error {
	switch code {
	case "NON_FAST_FORWARD":
		return fmt.Errorf("%w: %s", ErrNonFastForward, message)
	case "REPO_NOT_FOUND":
		return fmt.Errorf("%w: %s", ErrRepoNotFound, message)
	case "TIMELINE_UNKNOWN":
		return fmt.Errorf("%w: %s", ErrTimelineUnknown, message)
	case "PACK_NOT_FOUND":
		return fmt.Errorf("%w: %s", ErrPackNotFound, message)
	case "OBJECT_NOT_FOUND":
		return fmt.Errorf("%w: %s", model.ErrObjectNotFound, message)
	default:
		return fmt.Errorf("refs rpc %s: %s", code, message)
	}
}

func (c *RefsClient) call(ctx context.Context, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var we wireError
		if err := json.NewDecoder(resp.Body).Decode(&we); err == nil && we.Error.Code != "" {
			return mapCode(we.Error.Code, we.Error.Message)
		}
		return fmt.Errorf("refs rpc %s: status %d", path, resp.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// HealthyNodes lists storage nodes currently eligible for placement.
func (c *RefsClient) HealthyNodes(ctx context.Context) ([]NodeInfo, error) {
	var out struct {
		Nodes []NodeInfo `json:"nodes"`
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/internal/health/nodes", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("refs health: status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Nodes, nil
}

// GetPlacement reads a repo's replica target (0 = cluster default).
func (c *RefsClient) GetPlacement(ctx context.Context, repo string) (int, error) {
	var out struct {
		Replicas int `json:"replicas"`
	}
	if err := c.call(ctx, "/internal/meta/get-placement", map[string]string{"repo": repo}, &out); err != nil {
		return 0, err
	}
	return out.Replicas, nil
}

// --- RemoteMeta: MetaStore over the refs RPC ---

// RemoteMeta is the gateway's MetaStore (docs/10 §4): writes and strong
// reads go to the refs service; the interface is unchanged so gitwire, the
// service layer and the view engine never learn about the split.
type RemoteMeta struct {
	Refs *RefsClient
}

var _ MetaStore = (*RemoteMeta)(nil)

// HealthyNodes exposes the cluster health view through the MetaStore seam
// (service.ClusterNodes type-asserts for it).
func (m *RemoteMeta) HealthyNodes(ctx context.Context) ([]NodeInfo, error) {
	return m.Refs.HealthyNodes(ctx)
}

func (m *RemoteMeta) CreateRepo(ctx context.Context, repo string, algo model.HashAlgo) error {
	return m.Refs.call(ctx, "/internal/meta/create-repo",
		map[string]any{"repo": repo, "algo": int(algo)}, nil)
}

func (m *RemoteMeta) GetRepo(ctx context.Context, repo string) (RepoInfo, error) {
	var out struct {
		Repo RepoInfo `json:"repo"`
	}
	if err := m.Refs.call(ctx, "/internal/meta/get-repo", map[string]string{"repo": repo}, &out); err != nil {
		return RepoInfo{}, err
	}
	return out.Repo, nil
}

func (m *RemoteMeta) ListRepos(ctx context.Context) ([]RepoInfo, error) {
	var out struct {
		Repos []RepoInfo `json:"repos"`
	}
	if err := m.Refs.call(ctx, "/internal/meta/list-repos", map[string]string{}, &out); err != nil {
		return nil, err
	}
	return out.Repos, nil
}

// RenameRepo flips the registry on the refs service (docs/03 §6). Storage-
// layer moves (named addressing) have already happened at the caller — the
// refs service never touches pack bytes.
func (m *RemoteMeta) RenameRepo(ctx context.Context, from, to string) error {
	return m.Refs.call(ctx, "/internal/meta/rename-repo",
		map[string]string{"from": from, "to": to}, nil)
}

// SetForcePush toggles the repo-level force-push switch (docs/04 §6a).
func (m *RemoteMeta) SetForcePush(ctx context.Context, repo string, enabled bool) error {
	return m.Refs.call(ctx, "/internal/meta/set-force-push",
		map[string]any{"repo": repo, "enabled": enabled}, nil)
}

// ListHooks fetches the repo's receive-hook registry (docs/04 §7).
func (m *RemoteMeta) ListHooks(ctx context.Context, repo string) ([]HookInfo, error) {
	var out struct {
		Hooks []HookInfo `json:"hooks"`
	}
	if err := m.Refs.call(ctx, "/internal/meta/hooks",
		map[string]string{"repo": repo}, &out); err != nil {
		return nil, err
	}
	return out.Hooks, nil
}

func (m *RemoteMeta) SetHook(ctx context.Context, repo string, h HookInfo) error {
	body := map[string]any{"repo": repo, "hook": h}
	return m.Refs.call(ctx, "/internal/meta/hooks", body, nil)
}

func (m *RemoteMeta) DeleteHook(ctx context.Context, repo, id string) error {
	return m.Refs.call(ctx, "/internal/meta/hooks/delete",
		map[string]string{"repo": repo, "id": id}, nil)
}

func (m *RemoteMeta) PutPack(ctx context.Context, repo string, packOID model.OID, objects []model.OID, nodes []string) error {
	oids := make([]string, len(objects))
	for i, o := range objects {
		oids[i] = string(o)
	}
	return m.Refs.call(ctx, "/internal/meta/put-pack", map[string]any{
		"repo": repo, "pack_oid": string(packOID), "objects": oids, "nodes": nodes,
	}, nil)
}

func (m *RemoteMeta) GetPack(ctx context.Context, repo string, packOID model.OID) (PackInfo, error) {
	var out struct {
		Pack PackInfo `json:"pack"`
	}
	if err := m.Refs.call(ctx, "/internal/meta/get-pack",
		map[string]string{"repo": repo, "pack_oid": string(packOID)}, &out); err != nil {
		return PackInfo{}, err
	}
	return out.Pack, nil
}

func (m *RemoteMeta) ListPacks(ctx context.Context, repo string) ([]PackInfo, error) {
	var out struct {
		Packs []PackInfo `json:"packs"`
	}
	if err := m.Refs.call(ctx, "/internal/meta/list-packs", map[string]string{"repo": repo}, &out); err != nil {
		return nil, err
	}
	return out.Packs, nil
}

func (m *RemoteMeta) PackObjects(ctx context.Context, repo string, packOID model.OID) ([]model.OID, error) {
	var out struct {
		Objects []string `json:"objects"`
	}
	if err := m.Refs.call(ctx, "/internal/meta/pack-objects",
		map[string]string{"repo": repo, "pack_oid": string(packOID)}, &out); err != nil {
		return nil, err
	}
	objs := make([]model.OID, len(out.Objects))
	for i, o := range out.Objects {
		objs[i] = model.OID(o)
	}
	return objs, nil
}

func (m *RemoteMeta) DropPack(ctx context.Context, repo string, packOID model.OID) error {
	return m.Refs.call(ctx, "/internal/meta/drop-pack",
		map[string]string{"repo": repo, "pack_oid": string(packOID)}, nil)
}

func (m *RemoteMeta) SetPackNodes(ctx context.Context, repo string, packOID model.OID, nodes []string) error {
	return m.Refs.call(ctx, "/internal/meta/set-pack-nodes", map[string]any{
		"repo": repo, "pack_oid": string(packOID), "nodes": nodes,
	}, nil)
}

func (m *RemoteMeta) LocateObjects(ctx context.Context, repo string, oids []model.OID) (map[model.OID]ObjectLocation, error) {
	strs := make([]string, len(oids))
	for i, o := range oids {
		strs[i] = string(o)
	}
	var out struct {
		Found map[string]struct {
			Pack string `json:"pack"`
			Node string `json:"node"`
		} `json:"found"`
	}
	if err := m.Refs.call(ctx, "/internal/meta/locate",
		map[string]any{"repo": repo, "oids": strs}, &out); err != nil {
		return nil, err
	}
	res := make(map[model.OID]ObjectLocation, len(out.Found))
	for oid, loc := range out.Found {
		res[model.OID(oid)] = ObjectLocation{PackOID: model.OID(loc.Pack), NodeID: loc.Node}
	}
	return res, nil
}

func (m *RemoteMeta) Timeline(ctx context.Context, tl model.Timeline) (model.TimelineState, error) {
	var out struct {
		Head string `json:"head"`
		Base string `json:"base"`
	}
	err := m.Refs.call(ctx, "/internal/meta/timeline", map[string]string{
		"repo": tl.Repo, "anchor": string(tl.Anchor), "name": tl.Name,
	}, &out)
	if err != nil {
		return model.TimelineState{}, err
	}
	return model.TimelineState{Head: model.OID(out.Head), Base: model.OID(out.Base)}, nil
}

func (m *RemoteMeta) CASHead(ctx context.Context, tl model.Timeline, expected, next model.OID) error {
	return m.Refs.call(ctx, "/internal/meta/cas-head", map[string]string{
		"repo": tl.Repo, "anchor": string(tl.Anchor), "name": tl.Name,
		"expected": string(expected.NormalizeZero()), "next": string(next.NormalizeZero()),
	}, nil)
}

func (m *RemoteMeta) EnsureTimeline(ctx context.Context, tl model.Timeline, head, base model.OID) error {
	return m.Refs.call(ctx, "/internal/meta/ensure-timeline", map[string]string{
		"repo": tl.Repo, "anchor": string(tl.Anchor), "name": tl.Name,
		"head": string(head), "base": string(base),
	}, nil)
}

func (m *RemoteMeta) ListTimelines(ctx context.Context, repo string, anchor model.AnchorPath) ([]model.Timeline, error) {
	var out struct {
		Timelines []model.Timeline `json:"timelines"`
	}
	if err := m.Refs.call(ctx, "/internal/meta/list-timelines",
		map[string]string{"repo": repo, "anchor": string(anchor)}, &out); err != nil {
		return nil, err
	}
	return out.Timelines, nil
}

func (m *RemoteMeta) ListAllTimelines(ctx context.Context, repo string) ([]model.Timeline, error) {
	var out struct {
		Timelines []model.Timeline `json:"timelines"`
	}
	if err := m.Refs.call(ctx, "/internal/meta/list-all-timelines",
		map[string]string{"repo": repo}, &out); err != nil {
		return nil, err
	}
	return out.Timelines, nil
}

func (m *RemoteMeta) SetPlacement(ctx context.Context, repo string, replicas int) error {
	return m.Refs.call(ctx, "/internal/meta/set-placement",
		map[string]any{"repo": repo, "replicas": replicas}, nil)
}

func (m *RemoteMeta) GetPlacement(ctx context.Context, repo string) (int, error) {
	var out struct {
		Replicas int `json:"replicas"`
	}
	if err := m.Refs.call(ctx, "/internal/meta/get-placement",
		map[string]string{"repo": repo}, &out); err != nil {
		return 0, err
	}
	return out.Replicas, nil
}

// encodeIdxHeader packs idx bytes into a header value (storage PUT path).
func encodeIdxHeader(idx []byte) string {
	if len(idx) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(idx)
}

func decodeIdxHeader(v string) []byte {
	if v == "" {
		return nil
	}
	raw, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return nil
	}
	return raw
}
