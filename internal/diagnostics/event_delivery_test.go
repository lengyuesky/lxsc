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

func TestEventPaginationFieldsAreBoundedAndCopied(t *testing.T) {
	var events Events
	count, total, page, size := 30, 330, 4, 100
	events.Add(Event{Stage: "client_response", Count: &count, Total: &total, Page: &page, PageSize: &size, Error: "none"})
	count, total, page, size = 0, 0, 0, 0
	got := events.List()[0]
	if *got.Count != 30 || *got.Total != 330 || *got.Page != 4 || *got.PageSize != 100 {
		t.Fatalf("分页诊断字段未独立复制: %+v", got)
	}
	count, total, page, size = -1, 2000000, -1, 2000000
	events.Add(Event{Stage: "client_response", Count: &count, Total: &total, Page: &page, PageSize: &size, Error: "none"})
	got = events.List()[1]
	if *got.Count != 0 || *got.Total != 1000000 || *got.Page != 0 || *got.PageSize != 1000000 {
		t.Fatalf("分页诊断字段未按范围限制: %+v", got)
	}
}
