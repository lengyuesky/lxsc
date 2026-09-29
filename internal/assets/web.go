package assets

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type webAsset struct {
	plain, compressed    []byte
	version, contentType string
}

var resourceURL = regexp.MustCompile(`((?:src|href)=")([^"?]+\.(?:js|css))(")`)

// WebHandler 为嵌入资源生成内容版本、条件请求和预压缩，不引入额外运行时依赖。
func WebHandler() (http.Handler, error) {
	files := make(map[string]webAsset)
	entries, err := fs.ReadDir(Web, "web")
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := Web.ReadFile("web/" + entry.Name())
		if err != nil {
			return nil, err
		}
		files[entry.Name()] = webAsset{plain: data, version: fmt.Sprintf("%x", sha256.Sum256(data)), contentType: mime.TypeByExtension(path.Ext(entry.Name()))}
	}
	index := files["index.html"]
	index.plain = resourceURL.ReplaceAllFunc(index.plain, func(match []byte) []byte {
		parts := resourceURL.FindSubmatch(match)
		if asset, ok := files[string(parts[2])]; ok {
			return []byte(string(parts[1]) + string(parts[2]) + "?v=" + asset.version + string(parts[3]))
		}
		return match
	})
	index.version = fmt.Sprintf("%x", sha256.Sum256(index.plain))
	files["index.html"] = index
	for name, asset := range files {
		var compressed bytes.Buffer
		writer := gzip.NewWriter(&compressed)
		if _, err := writer.Write(asset.plain); err != nil {
			return nil, err
		}
		if err := writer.Close(); err != nil {
			return nil, err
		}
		asset.compressed = compressed.Bytes()
		files[name] = asset
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "不支持的方法", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "" {
			name = "index.html"
		}
		asset, ok := files[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Vary", "Accept-Encoding")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Type", asset.contentType)
		w.Header().Set("Cache-Control", "no-cache")
		if name != "index.html" && r.URL.Query().Get("v") == asset.version {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		data, etag := asset.plain, asset.version
		if r.Header.Get("Range") == "" && acceptsGzip(r.Header.Get("Accept-Encoding")) {
			data, etag = asset.compressed, etag+"-gzip"
			w.Header().Set("Content-Encoding", "gzip")
		}
		w.Header().Set("ETag", `"`+etag+`"`)
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
	}), nil
}

func acceptsGzip(value string) bool {
	wildcard := false
	for _, encoding := range strings.Split(value, ",") {
		parts := strings.Split(strings.TrimSpace(encoding), ";")
		quality := 1.0
		for _, param := range parts[1:] {
			key, value, ok := strings.Cut(strings.TrimSpace(param), "=")
			if ok && key == "q" {
				var err error
				quality, err = strconv.ParseFloat(value, 64)
				if err != nil || quality < 0 || quality > 1 {
					quality = 0
				}
			}
		}
		if strings.EqualFold(parts[0], "gzip") {
			return quality > 0
		}
		if parts[0] == "*" {
			wildcard = quality > 0
		}
	}
	return wildcard
}
