package cve

import (
	"math"
	"strings"
)

// cvssV3 computes the base score from FIRST's v3.0/v3.1 specification. Roundup
// uses integer precision as specified by v3.1, avoiding floating point overshoot.
func cvssV3(vector string) *CVSS {
	parts := strings.Split(vector, "/")
	if len(parts) < 9 || (parts[0] != "CVSS:3.1" && parts[0] != "CVSS:3.0") {
		return nil
	}
	m := map[string]string{}
	for _, s := range parts[1:] {
		k, v, ok := strings.Cut(s, ":")
		if !ok || m[k] != "" {
			return nil
		}
		m[k] = v
	}
	weights := map[string]map[string]float64{
		"AV": {"N": .85, "A": .62, "L": .55, "P": .2}, "AC": {"L": .77, "H": .44}, "UI": {"N": .85, "R": .62},
		"C": {"H": .56, "L": .22, "N": 0}, "I": {"H": .56, "L": .22, "N": 0}, "A": {"H": .56, "L": .22, "N": 0},
	}
	w := map[string]float64{}
	for k, values := range weights {
		v, ok := values[m[k]]
		if !ok {
			return nil
		}
		w[k] = v
	}
	if m["S"] != "U" && m["S"] != "C" {
		return nil
	}
	pr := map[string]float64{"N": .85, "L": .62, "H": .27}
	if m["S"] == "C" {
		pr = map[string]float64{"N": .85, "L": .68, "H": .5}
	}
	p, ok := pr[m["PR"]]
	if !ok {
		return nil
	}
	iss := 1 - (1-w["C"])*(1-w["I"])*(1-w["A"])
	impact := 6.42 * iss
	if m["S"] == "C" {
		impact = 7.52*(iss-.029) - 3.25*math.Pow(iss-.02, 15)
	}
	score := 0.0
	if impact > 0 {
		base := impact + 8.22*w["AV"]*w["AC"]*p*w["UI"]
		if m["S"] == "C" {
			base *= 1.08
		}
		base = math.Min(base, 10)
		score = math.Ceil(math.Round(base*100000)/10000) / 10
	}
	return &CVSS{Score: score, Vector: vector, Version: strings.TrimPrefix(parts[0], "CVSS:")}
}
func scoreSeverity(s float64) string {
	switch {
	case s >= 9:
		return "critical"
	case s >= 7:
		return "high"
	case s >= 4:
		return "medium"
	case s > 0:
		return "low"
	}
	return "unknown"
}
func severityRank(s string) int {
	return map[string]int{"critical": 4, "high": 3, "medium": 2, "low": 1}[s]
}
func vendorSeverity(s string) string {
	switch strings.ToLower(s) {
	case "critical":
		return "critical"
	case "high", "important":
		return "high"
	case "medium", "moderate":
		return "medium"
	case "low", "negligible", "unimportant":
		return "low"
	}
	return "unknown"
}
