package js

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestSDKInfoOnline(t *testing.T) {
	if os.Getenv("LXSC_ONLINE_TEST") == "" {
		t.Skip()
	}
	w := newTestWorker(t)
	if err := w.RunScript(context.Background(), "sdk.bundle.js", loadAsset(t, "sdk.bundle.js")); err != nil {
		t.Fatal(err)
	}
	for _, c := range [][2]string{{"wy", "186016"}, {"tx", "0039MnYb0qxYhV"}, {"kg", "B3A52A7A958BF0AED0EBFBA2E9A818B7"}, {"mg", "60054701923"}, {"kw", "228908"}} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		out, err := w.CallJSON(ctx, "__sdk_info", c[0], c[1])
		cancel()
		s := string(out)
		if len(s) > 200 {
			s = s[:200]
		}
		t.Logf("%s: %s %v", c[0], s, err)
	}
}
