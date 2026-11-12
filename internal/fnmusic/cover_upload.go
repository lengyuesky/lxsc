package fnmusic

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net/http"
	"strconv"
	"time"

	"lxsc/internal/admission"
	"lxsc/internal/coverart"
	"lxsc/internal/diagnostics"
)

const maxPlaylistCoverBytes = 5 << 20

func (s *Server) uploadPlaylistCover(w http.ResponseWriter, r *http.Request) {
	if !allowMusicWrite(w, r) {
		return
	}
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "multipart/form-data" {
		failure(w, http.StatusUnsupportedMediaType, "封面必须以表单上传")
		return
	}
	if s.Covers == nil {
		failure(w, http.StatusServiceUnavailable, "封面服务未配置")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), coverart.RequestTimeout)
	defer cancel()
	release, err := s.Covers.AcquireWork(ctx, currentUser(r).ID)
	if err != nil {
		w.Header().Set("Retry-After", "2")
		failure(w, http.StatusServiceUnavailable, "封面服务繁忙")
		return
	}
	defer release()
	r.Body = http.MaxBytesReader(w, r.Body, maxPlaylistCoverBytes+(64<<10))
	if err := r.ParseMultipartForm(maxPlaylistCoverBytes + (64 << 10)); err != nil {
		failure(w, http.StatusBadRequest, "封面上传无效或超过 5 MB")
		return
	}
	defer r.MultipartForm.RemoveAll()
	if len(r.MultipartForm.File) != 1 || len(r.MultipartForm.File["file"]) != 1 {
		failure(w, http.StatusBadRequest, "请上传一张封面图片")
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		failure(w, http.StatusBadRequest, "缺少封面图片")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxPlaylistCoverBytes+1))
	if err != nil || len(data) == 0 || len(data) > maxPlaylistCoverBytes {
		failure(w, http.StatusBadRequest, "封面上传无效或超过 5 MB")
		return
	}
	contentType = http.DetectContentType(data)
	switch contentType {
	case "image/jpeg", "image/png":
		config, _, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil || config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > 16_000_000 {
			failure(w, http.StatusBadRequest, "封面尺寸无效或过大")
			return
		}
		if _, _, err := image.Decode(bytes.NewReader(data)); err != nil {
			failure(w, http.StatusBadRequest, "封面图片不完整")
			return
		}
	case "image/webp":
		// 当前构建不含 WebP 解码器，不能把文件签名当作图片校验。
		failure(w, http.StatusUnsupportedMediaType, "请将 WEBP 封面转换为 JPEG 或 PNG 后上传")
		return
	default:
		failure(w, http.StatusUnsupportedMediaType, "只支持 JPEG 或 PNG 封面")
		return
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); mutationError(w, err) {
		return
	}
	id := hex.EncodeToString(raw[:])
	if mutationError(w, s.DB.SavePlaylistCover(r.Context(), id, currentUser(r).ID, contentType, data)) {
		return
	}
	success(w, map[string]any{"coverId": id})
}

func (s *Server) uploadedCover(w http.ResponseWriter, r *http.Request, event *diagnostics.Event) bool {
	id := r.URL.Query().Get("coverId")
	if !validGUID(id) {
		return false
	}
	if s.Covers == nil {
		failure(w, http.StatusServiceUnavailable, "封面交付未配置")
		return true
	}
	ctx, cancel := context.WithTimeout(r.Context(), coverart.RequestTimeout)
	defer cancel()
	release, err := s.Covers.AcquireDelivery(ctx)
	if err != nil {
		if errors.Is(err, admission.ErrBusy) {
			w.Header().Set("Retry-After", "2")
		}
		failure(w, http.StatusServiceUnavailable, "封面服务繁忙")
		return true
	}
	defer release()
	data, contentType, err := s.DB.ReadPlaylistCover(r.Context(), id, currentUser(r))
	if resourceError(w, err) {
		return true
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Referrer-Policy", "no-referrer")
	event.Result = "ok"
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(30 * time.Second))
	http.ServeContent(w, r, "cover", time.Time{}, bytes.NewReader(data))
	_ = http.NewResponseController(w).Flush()
	return true
}
