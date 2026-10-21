package model

import (
	"fmt"
	"strings"
)

// Tag is the minimal annotated-tag header parse — enough for closure
// traversal (tag → target object) and ref advertisement; the full body
// (tagger identity, message) stays opaque raw bytes.
type Tag struct {
	Object OID        // the tagged object (usually a commit)
	Type   ObjectType // the tagged object's type
	Name   string
}

// DecodeTag parses the annotated-tag header:
//
//	object <oid>
//	type <commit|tree|blob|tag>
//	tag <name>
//	tagger ...            ← opaque from here on
func DecodeTag(raw []byte) (*Tag, error) {
	var t Tag
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			break // end of header: tagger/message are opaque
		}
		key, val, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("decode tag: malformed header line %q", line)
		}
		switch key {
		case "object":
			t.Object = OID(strings.TrimSpace(val))
		case "type":
			t.Type = ObjectType(strings.TrimSpace(val))
		case "tag":
			t.Name = strings.TrimSpace(val)
		}
	}
	if t.Object.IsZero() {
		return nil, fmt.Errorf("decode tag: missing object header")
	}
	return &t, nil
}
