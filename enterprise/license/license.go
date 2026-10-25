// Package license is the enterprise plan gate: every commercial capability
// checks through here before serving. Licenses are Ed25519-signed tokens —
// base64url(payloadJSON) + "." + base64url(signature), no dependencies
// beyond the standard library — verified against the vendor public key the
// deployment configures. Absent a (valid) license the module runs on the
// FREE plan, which enables no enterprise features.
package license

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Plan is the subscription tier.
type Plan string

const (
	PlanFree       Plan = "free"
	PlanTeam       Plan = "team"
	PlanEnterprise Plan = "enterprise"
)

// Feature is one gated commercial capability. The set grows as features
// land in this module (audit stream, SSO, DB-backed policy, DR, ...).
type Feature string

const (
	FeatureAuditLog   Feature = "audit_log"  // full operation audit stream + SIEM export
	FeatureSSO        Feature = "sso"        // OIDC/SAML identity, token governance
	FeaturePolicyDB   Feature = "policy_db"  // database-backed policy source (auth.Service)
	FeatureDR         Feature = "dr"         // cross-cluster async replication, backup orchestration
	FeatureGovernance Feature = "governance" // compliance erasure, object GC, encryption at rest
	FeatureQuotas     Feature = "quotas"     // multi-tenancy, per-repo quota/rate limits
)

// planFeatures is the plan→feature matrix (the commercial catalog).
var planFeatures = map[Plan]map[Feature]bool{
	PlanFree:       {},
	PlanTeam:       {FeatureAuditLog: true},
	PlanEnterprise: {FeatureAuditLog: true, FeatureSSO: true, FeaturePolicyDB: true, FeatureDR: true, FeatureGovernance: true, FeatureQuotas: true},
}

// License is the verified content of a license token.
type License struct {
	Plan     Plan
	Instance string    // "*" or the licensed deployment instance id
	Seats    int       // 0 = unlimited
	Expires  time.Time // zero = non-expiring (dev licenses)
}

// Payload is the signed JSON body of a token.
type Payload struct {
	Plan     Plan   `json:"plan"`
	Instance string `json:"instance,omitempty"`
	Seats    int    `json:"seats,omitempty"`
	Expires  string `json:"expires,omitempty"` // RFC3339
}

// Service verifies licenses against one vendor public key.
type Service struct {
	pub ed25519.PublicKey
}

func NewService(pub ed25519.PublicKey) *Service { return &Service{pub: pub} }

// Issue signs a payload with a vendor private key (used by cmd/banlicense
// and tests).
func Issue(priv ed25519.PrivateKey, p Payload) (string, error) {
	body, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	sig := ed25519.Sign(priv, body)
	return base64.RawURLEncoding.EncodeToString(body) + "." +
		base64.RawURLEncoding.EncodeToString(sig), nil
}

// Parse verifies a token: signature against the vendor key, then expiry.
func (s *Service) Parse(token string) (*License, error) {
	body64, sig64, ok := strings.Cut(token, ".")
	if !ok {
		return nil, fmt.Errorf("license: malformed token")
	}
	body, err := base64.RawURLEncoding.DecodeString(body64)
	if err != nil {
		return nil, fmt.Errorf("license: payload: %w", err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(sig64)
	if err != nil {
		return nil, fmt.Errorf("license: signature: %w", err)
	}
	if len(sig) != ed25519.SignatureSize || !ed25519.Verify(s.pub, body, sig) {
		return nil, fmt.Errorf("license: bad signature")
	}
	var p Payload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("license: payload: %w", err)
	}
	lic := &License{Plan: p.Plan, Instance: p.Instance, Seats: p.Seats}
	if p.Expires != "" {
		exp, err := time.Parse(time.RFC3339, p.Expires)
		if err != nil {
			return nil, fmt.Errorf("license: expires: %w", err)
		}
		lic.Expires = exp
		if time.Now().After(exp) {
			return nil, fmt.Errorf("license: expired at %s", exp.Format(time.RFC3339))
		}
	}
	return lic, nil
}

// Enabled reports whether the license's plan carries the feature. A nil
// license (no valid token) is the FREE plan — nothing gated is enabled.
func Enabled(lic *License, f Feature) bool {
	return planFeatures[lic.plan()] != nil && planFeatures[lic.plan()][f]
}

// CoversInstance reports whether the license authorizes this deployment
// instance ("*" licenses any).
func (l *License) CoversInstance(instance string) bool {
	if l == nil || l.Instance == "" || l.Instance == "*" {
		return true
	}
	return l.Instance == instance
}

func (l *License) plan() Plan {
	if l == nil {
		return PlanFree
	}
	return l.Plan
}
