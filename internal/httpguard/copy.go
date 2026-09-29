package httpguard

import (
	"errors"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

var copyBuffers = sync.Pool{New: func() any { return make([]byte, 32<<10) }}

// CopyIdle 仅限制每次读取和写入的空闲时间，不限制整首音频的传输时长。
// 超时关闭上游正文以中止真实网络读取，不为每块数据创建等待协程。
func CopyIdle(w http.ResponseWriter, body io.ReadCloser, idle time.Duration) (int64, error) {
	buf := copyBuffers.Get().([]byte)
	defer copyBuffers.Put(buf)
	controller := http.NewResponseController(w)
	timer := time.AfterFunc(idle, func() { _ = body.Close() })
	defer timer.Stop()
	var written int64
	for {
		n, readErr := body.Read(buf)
		if !timer.Stop() {
			return written, os.ErrDeadlineExceeded
		}
		if n > 0 {
			if err := controller.SetWriteDeadline(time.Now().Add(idle)); err != nil && !errors.Is(err, http.ErrNotSupported) {
				return written, err
			}
			count, err := w.Write(buf[:n])
			written += int64(count)
			if err != nil {
				return written, err
			}
			if count != n {
				return written, io.ErrShortWrite
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return written, nil
			}
			return written, readErr
		}
		timer.Reset(idle)
	}
}
