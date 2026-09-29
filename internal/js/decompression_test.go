package js

import (
	"bytes"
	"strings"
	"testing"
)

func TestDecompressionLimitsAndRoundTrip(t *testing.T) {
	for _, pair := range [][2]string{{"deflate", "inflate"}, {"deflateRaw", "inflateRaw"}, {"gzip", "gunzip"}} {
		t.Run(pair[1], func(t *testing.T) {
			data := bytes.Repeat([]byte("a"), 1024)
			compressed, err := zlibOp(pair[0], data)
			if err != nil {
				t.Fatal(err)
			}
			out, err := zlibOpLimited(pair[1], compressed, 1024)
			if err != nil || !bytes.Equal(out, data) {
				t.Fatalf("边界大小的解压结果错误: %v", err)
			}
			out, err = zlibOpLimited(pair[1], compressed, 1023)
			if err == nil || !strings.Contains(err.Error(), "大小限制") || out != nil {
				t.Fatalf("超限必须报错且不返回部分数据: %v", err)
			}
			if _, err := zlibOpLimited(pair[1], []byte("损坏数据"), 1024); err == nil {
				t.Fatal("损坏的压缩流必须报错")
			}
		})
	}
	data, err := zlibOp("gzip", bytes.Repeat([]byte("b"), 600))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zlibOpLimited("gunzip", append(data, data...), 1024); err == nil {
		t.Fatal("多个 gzip 成员必须共享总大小限制")
	}
}
