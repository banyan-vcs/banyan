package auth

import (
	"encoding/base64"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

func demoPolicy() *StaticPolicy {
	return &StaticPolicy{
		Users:             map[string]string{"alice": "a", "bob": "b", "admin": "x"},
		Admins:            []string{"admin"},
		ProtectedBranches: []string{"main"},
		SubtreeAccess:     map[string][]string{"/secret": {"admin"}},
	}
}

func reqWithBasic(user, pass string) *http.Request {
	r, _ := http.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+pass)))
	return r
}

func TestStaticIdentify(t *testing.T) {
	p := demoPolicy()
	assert.True(t, p.Identify(&http.Request{}).Anonymous(), "无凭证=匿名")
	assert.True(t, p.Identify(reqWithBasic("alice", "wrong")).Anonymous(), "错密码=匿名")
	assert.Equal(t, Identity{User: "alice", Name: "alice"}, p.Identify(reqWithBasic("alice", "a")))
	assert.True(t, p.Identify(reqWithBasic("ghost", "a")).Anonymous(), "未知用户=匿名")
}

func TestStaticVisibleRefs(t *testing.T) {
	p := demoPolicy()
	refs := []Ref{
		{"refs/heads/main", "o1"},
		{"refs/heads/alice/wip", "o2"},
		{"refs/heads/alice/wip2", "o3"},
		{"refs/heads/bob/scratch", "o4"},
		{"refs/tags/v1", "o5"},
	}
	names := func(id Identity) []string {
		var out []string
		for _, r := range p.VisibleRefs(id, "demo", refs) {
			out = append(out, r.Name)
		}
		return out
	}
	assert.Equal(t, []string{"refs/heads/main", "refs/heads/alice/wip", "refs/heads/alice/wip2", "refs/tags/v1"},
		names(Identity{User: "alice"}), "alice 看不到 bob 的个人分支")
	assert.Equal(t, []string{"refs/heads/main", "refs/heads/bob/scratch", "refs/tags/v1"},
		names(Identity{User: "bob"}), "bob 看不到 alice 的个人分支")
	assert.Equal(t, []string{"refs/heads/main", "refs/tags/v1"},
		names(Identity{}), "匿名只过滤个人分支（认证由上层 401 把关）")
	all := names(Identity{User: "admin"})
	assert.Len(t, all, 5, "admin 看全部")
}

func TestStaticCanPush(t *testing.T) {
	p := demoPolicy()
	assert.False(t, p.CanPush(Identity{}, "demo", "", "dev"), "匿名不可 push")
	assert.True(t, p.CanPush(Identity{User: "alice"}, "demo", "", "dev"), "普通分支认证即可")
	assert.False(t, p.CanPush(Identity{User: "alice"}, "demo", "", "main"), "保护分支仅 admin")
	assert.True(t, p.CanPush(Identity{User: "admin"}, "demo", "", "main"), "admin 可推保护分支")
	assert.True(t, p.CanPush(Identity{User: "alice"}, "demo", "", "alice/wip"), "个人分支归主人")
	assert.False(t, p.CanPush(Identity{User: "bob"}, "demo", "", "alice/wip"), "他人个人分支拒绝")
	assert.True(t, p.CanPush(Identity{User: "admin"}, "demo", "", "alice/wip"), "admin 兜底")
	assert.True(t, p.CanPush(Identity{User: "alice"}, "demo", "/lib", "dev"), "子树仓库同规则")
}

func TestStaticCanFetch(t *testing.T) {
	p := demoPolicy()
	assert.False(t, p.CanFetch(Identity{}, "demo", ""), "匿名不可 fetch")
	assert.True(t, p.CanFetch(Identity{User: "bob"}, "demo", ""), "根仓库认证即可")
	assert.False(t, p.CanFetch(Identity{User: "bob"}, "demo", "/secret"), "受限目录拒绝")
	assert.True(t, p.CanFetch(Identity{User: "admin"}, "demo", "/secret"), "受限目录允许名单")
	assert.True(t, p.CanFetch(Identity{User: "bob"}, "demo", "/lib"), "未限目录开放")
}
