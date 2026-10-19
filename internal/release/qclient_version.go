package release

import (
	"strconv"
	"strings"
)

type qclientVersion struct {
	parts [5]uint64
	count int
}

// QClientVersionNewerThan compares the numeric versions recorded in Quilscan's
// qclient manifests. It accepts complete three-, four- and five-part versions;
// it never extracts a numeric prefix from a malformed manifest value.
//
// Quilscan's historical four-part releases 2.1.0.26 through 2.1.0.29 were built
// from the official 2.1.0.25 source line. When crossing into the five-part
// release scheme, compare those aliases as 2.1.0.25.0. Keep their original
// order when comparing two old releases. This policy is qclient-only: Node's
// ordinary version ordering must not reinterpret its patch numbers.
func QClientVersionNewerThan(candidate, current string) bool {
	a, okA := parseQClientVersion(candidate)
	b, okB := parseQClientVersion(current)
	if !okA || !okB {
		return false
	}
	if b.count == 5 && a.legacyAlias() {
		a.parts[3] = 25
	}
	if a.count == 5 && b.legacyAlias() {
		b.parts[3] = 25
	}
	for i := range a.parts {
		if a.parts[i] != b.parts[i] {
			return a.parts[i] > b.parts[i]
		}
	}
	return false
}

func (v qclientVersion) legacyAlias() bool {
	return v.count == 4 && v.parts[0] == 2 && v.parts[1] == 1 && v.parts[2] == 0 && v.parts[3] >= 26 && v.parts[3] <= 29
}

func parseQClientVersion(raw string) (qclientVersion, bool) {
	var v qclientVersion
	parts := strings.Split(strings.TrimSpace(raw), ".")
	if len(parts) < 3 || len(parts) > len(v.parts) {
		return v, false
	}
	v.count = len(parts)
	for i, part := range parts {
		if part == "" {
			return qclientVersion{}, false
		}
		for _, c := range part {
			if c < '0' || c > '9' {
				return qclientVersion{}, false
			}
		}
		n, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return qclientVersion{}, false
		}
		v.parts[i] = n
	}
	return v, true
}
