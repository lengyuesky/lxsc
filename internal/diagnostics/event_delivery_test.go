package diagnostics

import (
	"strings"
	"testing"
)

func TestEventDeliveryFieldsAreBoundedAndCopied(t *testing.T) {
	var events Events
	written, expected := int64(7), int64(12)
	events.Add(Event{Stage: "client_response", RequestID: "req-" + strings.Repeat("A", 26), BytesWritten: &written, ResponseBytes: &expected, Error: "write_error"})
	written, expected = 99, 100
	got := events.List()[0]
	if got.RequestID == "" || *got.BytesWritten != 7 || *got.ResponseBytes != 12 || got.Error != "write_error" {
		t.Fatalf("诊断计数未安全复制: %+v", got)
	}
	written, expected = -1, 1<<50
	events.Add(Event{Stage: "client_response", RequestID: "PRIVATE-request-header", BytesWritten: &written, ResponseBytes: &expected, Error: "encode_error"})
	got = events.List()[1]
	if got.RequestID != "" || *got.BytesWritten != 0 || *got.ResponseBytes != 1<<40 || got.Error != "encode_error" {
		t.Fatalf("诊断字段没有按白名单和范围处理: %+v", got)
	}
}
