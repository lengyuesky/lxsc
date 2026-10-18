package js

// CanonicalQuality 统一内部档位；调用脚本时仍使用其声明的别名。
func CanonicalQuality(q string) string {
	if q == "hires" {
		return "flac24bit"
	}
	return q
}

func QualityRank(q string) int {
	switch CanonicalQuality(q) {
	case "128k":
		return 1
	case "192k":
		return 2
	case "320k":
		return 3
	case "flac":
		return 4
	case "flac24bit":
		return 5
	case "master", "atmos", "atmos_plus", "dolby":
		return 6
	}
	return 0
}

// QualityChain 返回不高于请求的标准档位，特殊档位只尝试请求本身。
func QualityChain(want string) []string {
	want = CanonicalQuality(want)
	if QualityRank(want) == 0 {
		want = "320k"
	}
	var out []string
	if QualityRank(want) == 6 {
		out = append(out, want)
	}
	for _, q := range []string{"flac24bit", "flac", "320k", "192k", "128k"} {
		if QualityRank(q) <= QualityRank(want) {
			out = append(out, q)
		}
	}
	return out
}

func scriptQuality(q string, supported []string) (string, bool) {
	q = CanonicalQuality(q)
	for _, candidate := range supported {
		if candidate == q {
			return candidate, true
		}
	}
	for _, candidate := range supported {
		if CanonicalQuality(candidate) == q {
			return candidate, true
		}
	}
	return "", false
}
