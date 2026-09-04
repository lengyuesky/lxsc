package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"lxsc/internal/backup"
)

func (s *Server) backupStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Backup.Status())
}

func (s *Server) exportBackup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if r.Body != nil {
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
			_ = r.ParseForm()
			body.Password = r.FormValue("password")
		} else {
			_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body)
		}
	}
	ctx, cancel := contextWithTimeout(r, 30*time.Minute)
	defer cancel()
	archive, err := s.Backup.ExportLocal(ctx, body.Password)
	if err != nil {
		if strings.Contains(err.Error(), "正在执行") {
			fail(w, http.StatusConflict, err.Error())
		} else {
			fail(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	defer archive.Remove()
	f, err := os.Open(archive.Path)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, archive.Filename))
	w.Header().Set("Content-Length", fmt.Sprint(archive.Size))
	_, _ = io.Copy(w, f)
}

func contextWithTimeout(r *http.Request, duration time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), duration)
}

const maxBackupUpload = int64(2 << 30)

func (s *Server) saveBackupUpload(w http.ResponseWriter, r *http.Request) (target, filename string, cleanup func(), err error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBackupUpload+(16<<20))
	if err = r.ParseMultipartForm(8 << 20); err != nil {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
		return "", "", nil, fmt.Errorf("上传失败: %w", err)
	}
	root, err := os.MkdirTemp(s.Backup.DataDir, ".backup-upload-")
	if err != nil {
		r.MultipartForm.RemoveAll()
		return "", "", nil, err
	}
	cleanup = func() {
		r.MultipartForm.RemoveAll()
		_ = os.RemoveAll(root)
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		cleanup()
		return "", "", nil, errors.New("请选择备份文件")
	}
	defer file.Close()
	target = filepath.Join(root, "upload.lxsc-backup")
	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		cleanup()
		return "", "", nil, err
	}
	written, copyErr := io.Copy(out, io.LimitReader(file, maxBackupUpload+1))
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil {
		cleanup()
		return "", "", nil, errors.New("读取备份文件失败")
	}
	if written > maxBackupUpload {
		cleanup()
		return "", "", nil, errors.New("备份文件不能超过 2 GiB")
	}
	return target, filepath.Base(header.Filename), cleanup, nil
}

func (s *Server) inspectBackup(w http.ResponseWriter, r *http.Request) {
	target, _, cleanup, err := s.saveBackupUpload(w, r)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	defer cleanup()
	ctx, cancel := contextWithTimeout(r, 30*time.Minute)
	defer cancel()
	manifest, err := s.Backup.InspectArchive(ctx, target, r.FormValue("password"))
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"manifest": manifest})
}

func (s *Server) importBackup(w http.ResponseWriter, r *http.Request) {
	target, filename, cleanup, err := s.saveBackupUpload(w, r)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	defer cleanup()
	ctx, cancel := contextWithTimeout(r, 30*time.Minute)
	defer cancel()
	manifest, err := s.Backup.StageLocal(ctx, target, r.FormValue("password"), "本地文件: "+filename)
	if err != nil {
		code := http.StatusBadRequest
		if strings.Contains(err.Error(), "正在执行") {
			code = http.StatusConflict
		}
		fail(w, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"manifest": manifest, "pending": s.Backup.Status().Pending})
}

func (s *Server) cancelPendingBackup(w http.ResponseWriter, r *http.Request) {
	if err := s.Backup.CancelPending(); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) getWebDAVConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Backup.GetWebDAVConfig(r.Context()))
}

func (s *Server) putWebDAVConfig(w http.ResponseWriter, r *http.Request) {
	var cfg backup.WebDAVConfig
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&cfg); err != nil {
		fail(w, http.StatusBadRequest, "参数错误")
		return
	}
	out, err := s.Backup.SaveWebDAVConfig(r.Context(), cfg)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) testWebDAV(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 30*time.Second)
	defer cancel()
	if err := s.Backup.TestWebDAV(ctx); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) listWebDAVFiles(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 30*time.Second)
	defer cancel()
	files, err := s.Backup.ListRemote(ctx)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, files)
}

func (s *Server) runWebDAVBackup(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 30*time.Minute)
	defer cancel()
	if err := s.Backup.RunWebDAVBackup(ctx, "manual"); err != nil {
		code := http.StatusBadRequest
		if strings.Contains(err.Error(), "正在执行") {
			code = http.StatusConflict
		}
		fail(w, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.Backup.Status())
}

func (s *Server) downloadWebDAVFile(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	ctx, cancel := contextWithTimeout(r, 30*time.Minute)
	defer cancel()
	resp, err := s.Backup.OpenRemote(ctx, name)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
	if length := resp.Header.Get("Content-Length"); length != "" {
		w.Header().Set("Content-Length", length)
	}
	_, _ = io.Copy(w, resp.Body)
}

func (s *Server) restoreWebDAVFile(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body)
	ctx, cancel := contextWithTimeout(r, 30*time.Minute)
	defer cancel()
	manifest, err := s.Backup.RestoreRemote(ctx, chi.URLParam(r, "name"), body.Password)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"manifest": manifest, "pending": s.Backup.Status().Pending})
}

func (s *Server) deleteWebDAVFile(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 30*time.Second)
	defer cancel()
	if err := s.Backup.DeleteRemote(ctx, chi.URLParam(r, "name")); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
