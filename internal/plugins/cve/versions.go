package cve

import (
	"cmp"
	"strings"
)

func digit(c byte) bool  { return c >= '0' && c <= '9' }
func letter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func numeric(a, b string) int {
	a = strings.TrimLeft(a, "0")
	b = strings.TrimLeft(b, "0")
	if c := cmp.Compare(len(a), len(b)); c != 0 {
		return c
	}
	return strings.Compare(a, b)
}
func epoch(v string) (string, string) {
	if a, b, ok := strings.Cut(v, ":"); ok {
		return a, b
	}
	return "0", v
}

// debianCompare implements dpkg's epoch:upstream-revision ordering. Missing
// revisions compare as zero and tilde sorts before even the empty string.
func debianCompare(a, b string) int {
	ea, a := epoch(a)
	eb, b := epoch(b)
	if c := numeric(ea, eb); c != 0 {
		return c
	}
	split := func(v string) (string, string) {
		if n := strings.LastIndexByte(v, '-'); n >= 0 {
			return v[:n], v[n+1:]
		}
		return v, "0"
	}
	ua, ra := split(a)
	ub, rb := split(b)
	if c := debPart(ua, ub); c != 0 {
		return c
	}
	return debPart(ra, rb)
}
func debOrder(s string) int {
	if s == "" {
		return 0
	}
	c := s[0]
	if c == '~' {
		return -1
	}
	if digit(c) {
		return 0
	}
	if letter(c) {
		return int(c)
	}
	return int(c) + 256
}
func debPart(a, b string) int {
	for a != "" || b != "" {
		for len(a) > 0 && !digit(a[0]) || len(b) > 0 && !digit(b[0]) {
			if c := cmp.Compare(debOrder(a), debOrder(b)); c != 0 {
				return c
			}
			if a != "" {
				a = a[1:]
			}
			if b != "" {
				b = b[1:]
			}
		}
		i, j := 0, 0
		for i < len(a) && digit(a[i]) {
			i++
		}
		for j < len(b) && digit(b[j]) {
			j++
		}
		if c := numeric(a[:i], b[:j]); c != 0 {
			return c
		}
		a = a[i:]
		b = b[j:]
	}
	return 0
}

type apkToken struct {
	kind   int
	value  string
	suffix int
}

// The grammar is numeric components, an optional letter, ordered suffixes,
// optional commit hash, and -r revision. Noninitial leading-zero components
// compare lexically, as in apk-tools (rather than as decimal fractions).
func apkTokens(s string) []apkToken {
	out := []apkToken{}
	initial := true
	for len(s) > 0 {
		switch {
		case digit(s[0]):
			i := 0
			for i < len(s) && digit(s[i]) {
				i++
			}
			kind := 1
			if initial {
				kind = 0
			} else if len(out) > 0 && out[len(out)-1].kind == 3 {
				kind = 4
			}
			out = append(out, apkToken{kind: kind, value: s[:i]})
			s = s[i:]
		case s[0] == '.':
			s = s[1:]
		case s[0] == '_':
			s = s[1:]
			i := 0
			for i < len(s) && letter(s[i]) {
				i++
			}
			rank := map[string]int{"alpha": -4, "beta": -3, "pre": -2, "rc": -1, "cvs": 1, "svn": 2, "git": 3, "hg": 4, "p": 5}[s[:i]]
			out = append(out, apkToken{kind: 3, value: s[:i], suffix: rank})
			s = s[i:]
		case strings.HasPrefix(s, "-r"):
			out = append(out, apkToken{kind: 6, value: s[2:]})
			s = ""
		case s[0] == '~':
			out = append(out, apkToken{kind: 5, value: s[1:]})
			s = ""
		default:
			out = append(out, apkToken{kind: 2, value: s[:1]})
			s = s[1:]
		}
		initial = false
	}
	return append(out, apkToken{kind: 7})
}
func apkCompare(a, b string) int {
	as, bs := apkTokens(a), apkTokens(b)
	for i := 0; i < len(as) && i < len(bs); i++ {
		x, y := as[i], bs[i]
		if x.kind != y.kind {
			if x.kind == 3 && x.suffix < 0 {
				return -1
			}
			if y.kind == 3 && y.suffix < 0 {
				return 1
			}
			return cmp.Compare(y.kind, x.kind)
		}
		var c int
		switch x.kind {
		case 0, 4, 6:
			c = numeric(x.value, y.value)
		case 1:
			if strings.HasPrefix(x.value, "0") || strings.HasPrefix(y.value, "0") {
				c = strings.Compare(x.value, y.value)
			} else {
				c = numeric(x.value, y.value)
			}
		case 3:
			c = cmp.Compare(x.suffix, y.suffix)
		default:
			c = strings.Compare(x.value, y.value)
		}
		if c != 0 {
			return c
		}
	}
	return 0
}

// rpmCompare implements RPM's numeric/alphabetic segments, epoch, tilde and
// caret ordering without invoking rpm or a shell.
func rpmCompare(a, b string) int {
	ea, a := epoch(a)
	eb, b := epoch(b)
	if c := numeric(ea, eb); c != 0 {
		return c
	}
	for a != "" || b != "" {
		for len(a) > 0 && !letter(a[0]) && !digit(a[0]) && a[0] != '~' && a[0] != '^' {
			a = a[1:]
		}
		for len(b) > 0 && !letter(b[0]) && !digit(b[0]) && b[0] != '~' && b[0] != '^' {
			b = b[1:]
		}
		has := func(s string, c byte) bool { return len(s) > 0 && s[0] == c }
		if has(a, '~') || has(b, '~') {
			if !has(a, '~') {
				return 1
			}
			if !has(b, '~') {
				return -1
			}
			a = a[1:]
			b = b[1:]
			continue
		}
		if has(a, '^') || has(b, '^') {
			if a == "" {
				return -1
			}
			if b == "" {
				return 1
			}
			if !has(a, '^') {
				return 1
			}
			if !has(b, '^') {
				return -1
			}
			a = a[1:]
			b = b[1:]
			continue
		}
		if a == "" || b == "" {
			return cmp.Compare(len(a), len(b))
		}
		na, nb := digit(a[0]), digit(b[0])
		if na != nb {
			if na {
				return 1
			}
			return -1
		}
		i, j := 0, 0
		for i < len(a) && (na && digit(a[i]) || !na && letter(a[i])) {
			i++
		}
		for j < len(b) && (nb && digit(b[j]) || !nb && letter(b[j])) {
			j++
		}
		c := strings.Compare(a[:i], b[:j])
		if na {
			c = numeric(a[:i], b[:j])
		}
		if c != 0 {
			return c
		}
		a = a[i:]
		b = b[j:]
	}
	return 0
}
func versionCompare(eco, a, b string) int {
	if strings.HasPrefix(eco, "Alpine:") {
		return apkCompare(a, b)
	}
	if strings.HasPrefix(eco, "Rocky Linux:") || strings.HasPrefix(eco, "AlmaLinux:") {
		return rpmCompare(a, b)
	}
	return debianCompare(a, b)
}
