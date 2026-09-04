// Package subsonic 实现 Subsonic / OpenSubsonic REST API
package subsonic

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// APIVersion 声明的协议版本
const APIVersion = "1.16.1"

// ServerVersion 服务端版本
var ServerVersion = "0.1.0"

// 错误码
const (
	ErrGeneric        = 0
	ErrMissingParam   = 10
	ErrClientOld      = 20
	ErrServerOld      = 30
	ErrWrongAuth      = 40
	ErrTokenAuthNotOK = 41
	ErrNotAuthorized  = 50
	ErrTrial          = 60
	ErrNotFound       = 70
)

// M 有序对象：使用 map 但输出时排序键，便于 XML 属性与 JSON 稳定
type M map[string]any

// Response 包装
type Response struct {
	body   M
	format string
	name   string
}

func detectFormat(r *http.Request) string {
	f := strings.ToLower(param(r, "f"))
	if f == "json" || f == "jsonp" {
		return f
	}
	return "xml"
}

// writeOK 输出成功响应；payload 为顶层元素名与内容（可为空）
func writeOK(w http.ResponseWriter, r *http.Request, name string, payload any) {
	writeResp(w, r, "ok", name, payload, nil)
}

func writeErr(w http.ResponseWriter, r *http.Request, code int, msg string) {
	writeResp(w, r, "failed", "error", nil, M{"code": code, "message": msg})
}

func writeResp(w http.ResponseWriter, r *http.Request, status, name string, payload any, errObj M) {
	format := detectFormat(r)
	root := M{
		"status":        status,
		"version":       APIVersion,
		"type":          "lxsc",
		"serverVersion": ServerVersion,
		"openSubsonic":  true,
	}
	if errObj != nil {
		root["error"] = errObj
	} else if name != "" && payload != nil {
		root[name] = payload
	}
	switch format {
	case "json", "jsonp":
		b, _ := json.Marshal(M{"subsonic-response": root})
		if format == "jsonp" {
			cb := param(r, "callback")
			if cb == "" {
				cb = "callback"
			}
			w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
			w.Write([]byte(cb + "("))
			w.Write(b)
			w.Write([]byte(");"))
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Write(b)
	default:
		root["xmlns"] = "http://subsonic.org/restapi"
		var buf bytes.Buffer
		buf.WriteString(xml.Header)
		writeXML(&buf, "subsonic-response", root)
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.Write(buf.Bytes())
	}
}

// writeXML 将 M 树写为 XML：标量成为属性，对象/数组成为子元素，"value" 键成为文本内容
func writeXML(buf *bytes.Buffer, name string, v any) {
	switch t := v.(type) {
	case M:
		writeXMLObj(buf, name, map[string]any(t))
	case map[string]any:
		writeXMLObj(buf, name, t)
	case []M:
		for _, it := range t {
			writeXMLObj(buf, name, map[string]any(it))
		}
	case []any:
		for _, it := range t {
			writeXML(buf, name, it)
		}
	case []map[string]any:
		for _, it := range t {
			writeXMLObj(buf, name, it)
		}
	default:
		buf.WriteString("<" + name + ">")
		xml.EscapeText(buf, []byte(fmt.Sprint(v)))
		buf.WriteString("</" + name + ">")
	}
}

func writeXMLObj(buf *bytes.Buffer, name string, obj map[string]any) {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	buf.WriteString("<" + name)
	var children []string
	var text string
	hasText := false
	for _, k := range keys {
		v := obj[k]
		if v == nil {
			continue
		}
		switch v.(type) {
		case M, map[string]any, []M, []any, []map[string]any, []string, []int:
			children = append(children, k)
		default:
			if k == "value" {
				text = fmt.Sprint(v)
				hasText = true
				continue
			}
			buf.WriteString(" " + k + "=\"")
			xml.EscapeText(buf, []byte(fmt.Sprint(v)))
			buf.WriteString("\"")
		}
	}
	if len(children) == 0 && !hasText {
		buf.WriteString("/>")
		return
	}
	buf.WriteString(">")
	if hasText {
		xml.EscapeText(buf, []byte(text))
	}
	for _, k := range children {
		switch t := obj[k].(type) {
		case []string:
			for _, s := range t {
				buf.WriteString("<" + k + ">")
				xml.EscapeText(buf, []byte(s))
				buf.WriteString("</" + k + ">")
			}
		case []int:
			for _, n := range t {
				buf.WriteString(fmt.Sprintf("<%s>%d</%s>", k, n, k))
			}
		default:
			writeXML(buf, k, t)
		}
	}
	buf.WriteString("</" + name + ">")
}

// param 从查询参数或表单读取（支持 formPost）
func param(r *http.Request, key string) string {
	if v := r.URL.Query().Get(key); v != "" {
		return v
	}
	if r.Method == http.MethodPost {
		return r.PostFormValue(key)
	}
	return ""
}

func params(r *http.Request, key string) []string {
	vals := r.URL.Query()[key]
	if r.Method == http.MethodPost {
		if r.PostForm == nil {
			_ = r.ParseForm()
		}
		vals = append(vals, r.PostForm[key]...)
	}
	return vals
}

func paramInt(r *http.Request, key string, def int) int {
	v := param(r, key)
	if v == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		return def
	}
	return n
}
