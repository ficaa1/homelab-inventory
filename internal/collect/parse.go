package collect

import (
	"strconv"
	"strings"
)

// parseKeyValue splits a document of "key<sep>value" lines into a map,
// skipping blanks and # comments. Used for /etc/os-release and friends.
func parseKeyValue(s, sep string) map[string]string {
	out := make(map[string]string)
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, sep)
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out
}

// unquote strips one layer of matching single or double quotes.
func unquote(s string) string {
	if len(s) >= 2 {
		if q := s[0]; (q == '"' || q == '\'') && s[len(s)-1] == q {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// splitFieldsN splits on whitespace into at most n fields, leaving the
// remainder (typically a description with spaces) intact as the last element.
func splitFieldsN(s string, n int) []string {
	var out []string
	rest := strings.TrimLeft(s, " \t")
	for len(out) < n-1 {
		i := strings.IndexAny(rest, " \t")
		if i < 0 {
			break
		}
		out = append(out, rest[:i])
		rest = strings.TrimLeft(rest[i:], " \t")
	}
	if rest != "" {
		out = append(out, rest)
	}
	return out
}

// atoi is strconv.Atoi with a zero default, for fields where a parse failure
// is not worth surfacing.
func atoi(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}
