package main

import "testing"

// --scenarios parsing: empty = run everything (nil), names are validated,
// and the returned map is assignable (the nil-map panic the git.git run
// caught before its first scenario).
func TestParseScenarios(t *testing.T) {
	if m, ok := parseScenarios(""); !ok || m != nil {
		t.Fatalf("empty filter: ok=%v map=%v, want nil,true", ok, m)
	}
	m, ok := parseScenarios("clone-full, ls-remote ,burst")
	if !ok || len(m) != 3 || !m["clone-full"] || !m["ls-remote"] || !m["burst"] {
		t.Fatalf("parse: ok=%v map=%v", ok, m)
	}
	if m, ok := parseScenarios("clone-full,nope"); ok || m != nil {
		t.Fatalf("unknown family should fail: ok=%v map=%v", ok, m)
	}
}
