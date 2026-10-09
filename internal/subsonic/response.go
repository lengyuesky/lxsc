// Package subsonic 实现 Subsonic / OpenSubsonic REST API
package subsonic

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"lxsc/internal/diagnostics"
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
	ErrBusy           = -1 // 仅内部区分过载，对外仍使用兼容的通用错误码。
	ErrAuthLimited    = -2 // 认证限频使用 HTTP 429，协议错误保持兼容。
	ErrUnavailable    = -3 // 上游暂时不可用，可稍后重试，不代表资源为空。
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
	status, retry := http.StatusOK, ""
	if code == ErrBusy || code == ErrAuthLimited || code == ErrUnavailable {
		status, retry = http.StatusServiceUnavailable, "2"
		if code == ErrAuthLimited {
			status, retry = http.StatusTooManyRequests, "60"
		}
		code = ErrGeneric
	} else if r.Method == http.MethodHead {
		// HEAD 没有协议正文，必须用 HTTP 状态表示失败，不能返回空的 200 错误页。
		switch code {
		case ErrMissingParam, ErrClientOld, ErrServerOld, 43:
			status = http.StatusBadRequest
		case ErrWrongAuth, ErrTokenAuthNotOK, 44:
			status = http.StatusUnauthorized
		case ErrNotAuthorized:
			status = http.StatusForbidden
		case ErrNotFound:
			status = http.StatusNotFound
		default:
			status = http.StatusBadGateway
		}
	}
	if retry != "" {
		w.Header().Set("Retry-After", retry)
	}
	if r.Method == http.MethodHead {
		w.Header().Set("Cache-Control", "no-store")
	}
	writeRespStatus(w, r, status, "failed", "error", nil, M{"code": code, "message": msg})
}

func writeResp(w http.ResponseWriter, r *http.Request, status, name string, payload any, errObj M) {
	writeRespStatus(w, r, http.StatusOK, status, name, payload, errObj)
}

func writeRespStatus(w http.ResponseWriter, r *http.Request, httpStatus int, status, name string, payload any, errObj M) {
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
	body, contentType, encodeErr := encodeResponse(r, format, root)
	issue := ""
	if encodeErr != nil {
		// 正文已替换为固定协议错误，不泄露序列化失败的字段或原始错误文本。
		httpStatus, status, name, payload = http.StatusInternalServerError, "failed", "error", nil
		errObj = M{"code": ErrGeneric}
		issue = "encode_error"
	}
	w.Header().Set("Content-Type", contentType)
	// 正文已经完整序列化，显式长度让 HTTP/1 客户端及反代读完正文即可
	// 确认消息完整，不必继续等待 handler 收尾后才发出的分块结束标记。
	if r.Method != http.MethodHead {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	}
	if event, _ := r.Context().Value(clientDiagnosticKey{}).(*diagnostics.Event); event != nil {
		event.Status = httpStatus
		if event.RequestID != "" {
			w.Header().Set("X-Request-ID", event.RequestID)
		}
	}
	w.WriteHeader(httpStatus)
	n, expected := 0, 0
	if r.Method != http.MethodHead {
		expected = len(body)
		var err error
		n, err = w.Write(body)
		if err != nil || n != expected {
			issue = "write_error"
		}
	}
	// Write 可能只把数据放入缓冲区；小响应和最后一段正文都需在请求
	// 收尾前刷出。主动协议探测的内存 writer 不支持 Flush，保持兼容。
	if err := http.NewResponseController(w).Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		issue = "write_error"
	}
	// 刷新之后再记录，短写或刷新失败都不能被标记为成功交付。
	recordClientResponse(r, status, name, payload, errObj)
	recordClientDelivery(r, n, expected, issue)
}

func encodeResponse(r *http.Request, format string, root M) ([]byte, string, error) {
	switch format {
	case "json", "jsonp":
		b, err := json.Marshal(M{"subsonic-response": root})
		if err != nil {
			// 此固定错误正文不包含原 payload，也不再依赖可能失败的通用序列化。
			b = []byte(`{"subsonic-response":{"status":"failed","version":` + strconv.Quote(APIVersion) + `,"error":{"code":0,"message":"响应生成失败，请稍后重试"}}}`)
		}
		if format == "jsonp" {
			cb := param(r, "callback")
			if cb == "" {
				cb = "callback"
			}
			return []byte(cb + "(" + string(b) + ");"), "application/javascript; charset=utf-8", err
		}
		return b, "application/json; charset=utf-8", err
	default:
		root["xmlns"] = "http://subsonic.org/restapi"
		var buf bytes.Buffer
		buf.WriteString(xml.Header)
		writeXML(&buf, "subsonic-response", root)
		return buf.Bytes(), "application/xml; charset=utf-8", nil
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
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// 分页数量包含零（客户端可只请求专辑或歌手），单次最多返回 500 项。
func paramCount(r *http.Request, key string, def int) int {
	return min(500, max(0, paramInt(r, key, def)))
}

func paramOffset(r *http.Request, key string) int {
	// 给后续页码计算保留空间，避免极大整数相加溢出。
	return min(int(^uint(0)>>1)-500, max(0, paramInt(r, key, 0)))
}
