package license

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLicenseRoundTrip(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	svc := NewService(pub)

	tok, err := Issue(priv, Payload{Plan: PlanEnterprise, Instance: "*", Seats: 50})
	require.NoError(t, err)
	lic, err := svc.Parse(tok)
	require.NoError(t, err)
	assert.Equal(t, PlanEnterprise, lic.Plan)
	assert.True(t, lic.CoversInstance("any-host"), "* 授权任意实例")
	assert.True(t, Enabled(lic, FeatureAuditLog))
	assert.True(t, Enabled(lic, FeatureDR))
	assert.False(t, Enabled(&License{Plan: PlanFree}, FeatureAuditLog), "free 无企业特性")
	assert.False(t, Enabled(&License{Plan: PlanTeam}, FeatureSSO), "team 不含 SSO")
	assert.True(t, Enabled(&License{Plan: PlanTeam}, FeatureAuditLog))
}

func TestLicenseTamperAndExpiry(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	svc := NewService(pub)

	tok, err := Issue(priv, Payload{Plan: PlanTeam})
	require.NoError(t, err)
	tampered := tok[:len(tok)-4] + "AAAA"
	_, err = svc.Parse(tampered)
	require.Error(t, err, "篡改的签名应被拒")

	otherPub, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	wrongKey, err := Issue(otherPriv, Payload{Plan: PlanEnterprise})
	require.NoError(t, err)
	_, err = svc.Parse(wrongKey)
	require.Error(t, err, "错误私钥签发的许可应被拒")
	_, err = NewService(otherPub).Parse(tok)
	require.Error(t, err, "不同签发方公钥应拒正确 token")

	expired, err := Issue(priv, Payload{Plan: PlanEnterprise, Expires: time.Now().Add(-time.Hour).Format(time.RFC3339)})
	require.NoError(t, err)
	_, err = svc.Parse(expired)
	require.Error(t, err, "过期许可应被拒")
}

func TestNilLicenseIsFree(t *testing.T) {
	assert.False(t, Enabled(nil, FeatureAuditLog), "无许可 = FREE，无企业特性")
	var lic *License
	assert.True(t, lic.CoversInstance("x"), "无许可实例校验不额外拒绝")
}
