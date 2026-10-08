package diagnostics

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
)

var diagnosticURL = regexp.MustCompile(`\b[A-Za-z][A-Za-z0-9+.-]{1,15}://[^\s<>"']+`)
var diagnosticBearer = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/-]+=*`)
var diagnosticKey = regexp.MustCompile(`\b(?:lxdbg_|lxsc_)[A-Za-z0-9_-]{16,}`)
var diagnosticAssignment = regexp.MustCompile(`(?i)["']?\b(password|passwd|pwd|token|api[_-]?key|secret|authorization|cookie|set-cookie)["']?\s*[:=]\s*("[^"\r\n]*"|'[^'\r\n]*'|[^\s,;]+)`)

func AddressSummary(address string) map[string]any {
	if address == "" {
		return map[string]any{"configured": false}
	}
	u, err := url.Parse(address)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return map[string]any{"configured": true, "valid": false}
	}
	return map[string]any{"configured": true, "valid": true, "scheme": u.Scheme, "host": u.Host, "credentialsConfigured": u.User != nil}
}

// RedactText 用于管理员明确授权的完整诊断；原始自由文本不是可证明无秘密的数据。
// 去除可识别凭据和 URL 的路径/参数，并限制单字段大小。旧 read/probe 仍只使用固定字段。
func RedactText(value string) string {
	value = diagnosticURL.ReplaceAllStringFunc(value, func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return "[url]"
		}
		return u.Scheme + "://" + u.Host + "/[redacted]"
	})
	value = diagnosticBearer.ReplaceAllString(value, "Bearer [redacted]")
	value = diagnosticKey.ReplaceAllString(value, "[redacted]")
	value = diagnosticAssignment.ReplaceAllString(value, "$1=[redacted]")
	if len(value) > 4096 {
		value = string([]rune(value)[:min(2048, len([]rune(value)))]) + "…"
	}
	return value
}

func sensitiveField(key string) bool {
	key = strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(key))
	for _, part := range []string{"password", "passwd", "token", "secret", "apikey", "authorization", "cookie", "privatekey"} {
		if strings.Contains(key, part) {
			return true
		}
	}
	return key == "script" || key == "scriptcontent" || key == "p" || key == "t" || key == "s"
}

// RedactValue 返回独立的 JSON 值，不修改业务配置、日志或 SDK 缓存。
func RedactValue(value any) any {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var decoded any
	if json.Unmarshal(raw, &decoded) != nil {
		return nil
	}
	var walk func(any) any
	walk = func(v any) any {
		switch v := v.(type) {
		case map[string]any:
			for key, child := range v {
				if sensitiveField(key) {
					v[key] = "[redacted]"
				} else {
					v[key] = walk(child)
				}
			}
		case []any:
			for i, child := range v {
				v[i] = walk(child)
			}
		case string:
			return RedactText(v)
		}
		return v
	}
	return walk(decoded)
}
