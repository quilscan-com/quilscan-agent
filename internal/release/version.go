package release

import (
	"regexp"
	"strconv"
	"strings"
)

var dottedVersionPattern = regexp.MustCompile(`\b(?:node-|qclient-)?([0-9]+(?:\.[0-9]+){2,3})(?:-[A-Za-z0-9_-]+)?\b`)

func VersionNewerThan(candidate, current string) bool {
	return CompareDottedVersions(candidate, current) > 0
}

func CompareDottedVersions(a, b string) int {
	pa, okA := ParseDottedVersion(a)
	pb, okB := ParseDottedVersion(b)
	if !okA || !okB {
		return 0
	}
	for i := range pa {
		if pa[i] > pb[i] {
			return 1
		}
		if pa[i] < pb[i] {
			return -1
		}
	}
	return 0
}

func ParseDottedVersion(v string) ([4]int, bool) {
	var out [4]int
	m := dottedVersionPattern.FindStringSubmatch(v)
	if len(m) != 2 {
		return out, false
	}
	parts := strings.Split(m[1], ".")
	if len(parts) < 3 || len(parts) > 4 {
		return out, false
	}
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
