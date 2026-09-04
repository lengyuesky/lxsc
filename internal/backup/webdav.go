package backup

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var backupNamePattern = regexp.MustCompile(`^lxsc-[A-Za-z0-9_.-]+\.lxsc-backup$`)

// RemoteFile 是 WebDAV 中的备份文件。
type RemoteFile struct {
	Name         string `json:"name"`
	Size         int64  `json:"size"`
	LastModified int64  `json:"lastModified"`
}

type davClient struct {
	http     *http.Client
	base     *url.URL
	username string
	password string
}

func newDAVClient(client *http.Client, rawURL, username, password string) (*davClient, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("WebDAV 地址无效")
	}
	clone := *client
	originalCheck := clone.CheckRedirect
	origin := webDAVOrigin(u)
	clone.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		// 只允许完全同源重定向，尤其禁止 HTTPS 降级到 HTTP 后继续携带认证信息。
		if webDAVOrigin(req.URL) != origin {
			return http.ErrUseLastResponse
		}
		if originalCheck != nil {
			return originalCheck(req, via)
		}
		if len(via) >= 10 {
			return errors.New("重定向次数过多")
		}
		return nil
	}
	return &davClient{http: &clone, base: u, username: username, password: password}, nil
}

func webDAVOrigin(u *url.URL) string {
	port := u.Port()
	if port == "" {
		if strings.EqualFold(u.Scheme, "https") {
			port = "443"
		} else if strings.EqualFold(u.Scheme, "http") {
			port = "80"
		}
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Hostname()) + ":" + port
}

func (d *davClient) target(parts ...string) string {
	u := *d.base
	joined := strings.TrimSuffix(u.Path, "/")
	for _, part := range parts {
		joined = path.Join(joined, strings.Trim(part, "/"))
	}
	u.Path = joined
	return u.String()
}

func (d *davClient) request(ctx context.Context, method, target string, body io.Reader, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	if d.username != "" || d.password != "" {
		req.SetBasicAuth(d.username, d.password)
	}
	req.Header.Set("User-Agent", "lxsc-backup")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return d.http.Do(req)
}

func (d *davClient) ensureDir(ctx context.Context, dir string) error {
	parts := strings.Split(strings.Trim(dir, "/"), "/")
	var current []string
	for _, part := range parts {
		if part == "" {
			continue
		}
		current = append(current, part)
		resp, err := d.request(ctx, "MKCOL", d.target(strings.Join(current, "/")), nil, nil)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusMethodNotAllowed && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusConflict {
			return fmt.Errorf("创建 WebDAV 目录失败: %s", resp.Status)
		}
	}
	return nil
}

func (d *davClient) test(ctx context.Context, dir string) error {
	if err := d.ensureDir(ctx, dir); err != nil {
		return err
	}
	resp, err := d.request(ctx, "PROPFIND", d.target(dir), strings.NewReader(`<?xml version="1.0"?><propfind xmlns="DAV:"><prop><resourcetype/></prop></propfind>`), map[string]string{"Depth": "0", "Content-Type": "application/xml"})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 207 && resp.StatusCode != 200 {
		return fmt.Errorf("WebDAV 连接测试失败: %s", resp.Status)
	}
	return nil
}

func (d *davClient) put(ctx context.Context, dir, name, file string) error {
	if err := d.ensureDir(ctx, dir); err != nil {
		return err
	}
	in, err := os.Open(file)
	if err != nil {
		return err
	}
	defer in.Close()
	info, _ := in.Stat()
	resp, err := d.request(ctx, http.MethodPut, d.target(dir, name), in, map[string]string{"Content-Type": "application/octet-stream", "Content-Length": strconv.FormatInt(info.Size(), 10)})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("上传失败: %s %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

type multiStatus struct {
	Responses []davResponse `xml:"response"`
}
type davResponse struct {
	Href      string `xml:"href"`
	Propstats []struct {
		Prop struct {
			Length   string `xml:"getcontentlength"`
			Modified string `xml:"getlastmodified"`
			Resource struct {
				Collection *struct{} `xml:"collection"`
			} `xml:"resourcetype"`
		} `xml:"prop"`
	} `xml:"propstat"`
}

func (d *davClient) list(ctx context.Context, dir string) ([]RemoteFile, error) {
	body := bytes.NewBufferString(`<?xml version="1.0"?><propfind xmlns="DAV:"><prop><getcontentlength/><getlastmodified/><resourcetype/></prop></propfind>`)
	resp, err := d.request(ctx, "PROPFIND", d.target(dir), body, map[string]string{"Depth": "1", "Content-Type": "application/xml"})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 207 && resp.StatusCode != 200 {
		return nil, fmt.Errorf("读取远程备份失败: %s", resp.Status)
	}
	var ms multiStatus
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&ms); err != nil {
		return nil, err
	}
	var out []RemoteFile
	for _, item := range ms.Responses {
		u, _ := url.Parse(item.Href)
		name, _ := url.PathUnescape(path.Base(strings.TrimSuffix(u.Path, "/")))
		if !backupNamePattern.MatchString(name) {
			continue
		}
		file := RemoteFile{Name: name}
		for _, ps := range item.Propstats {
			if ps.Prop.Resource.Collection != nil {
				file.Name = ""
				break
			}
			if n, err := strconv.ParseInt(ps.Prop.Length, 10, 64); err == nil {
				file.Size = n
			}
			if t, err := http.ParseTime(ps.Prop.Modified); err == nil {
				file.LastModified = t.Unix()
			}
		}
		if file.Name != "" {
			out = append(out, file)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastModified != out[j].LastModified {
			return out[i].LastModified > out[j].LastModified
		}
		return out[i].Name > out[j].Name
	})
	return out, nil
}

func (d *davClient) get(ctx context.Context, dir, name, target string) error {
	if !backupNamePattern.MatchString(name) {
		return errors.New("远程文件名无效")
	}
	resp, err := d.request(ctx, http.MethodGet, d.target(dir, name), nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载失败: %s", resp.Status)
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	written, err := io.Copy(out, io.LimitReader(resp.Body, maxUnpackedSize+1))
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err == nil && written > maxUnpackedSize {
		err = errors.New("远程备份文件超过大小限制")
	}
	return err
}

func (d *davClient) delete(ctx context.Context, dir, name string) error {
	if !backupNamePattern.MatchString(name) {
		return errors.New("远程文件名无效")
	}
	resp, err := d.request(ctx, http.MethodDelete, d.target(dir, name), nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("删除失败: %s", resp.Status)
	}
	return nil
}

func (s *Service) dav(ctx context.Context) (*davClient, WebDAVConfig, string, error) {
	cfg, password, backupPassword, err := s.webDAVSecrets(ctx)
	if err != nil {
		return nil, cfg, "", err
	}
	if cfg.URL == "" {
		return nil, cfg, "", errors.New("请先配置 WebDAV 地址")
	}
	client, err := newDAVClient(s.HTTP, cfg.URL, cfg.Username, password)
	return client, cfg, backupPassword, err
}

// TestWebDAV 测试认证与目录读权限。
func (s *Service) TestWebDAV(ctx context.Context) error {
	client, cfg, _, err := s.dav(ctx)
	if err != nil {
		return err
	}
	return client.test(ctx, cfg.Path)
}

// ListRemote 列出远程备份。
func (s *Service) ListRemote(ctx context.Context) ([]RemoteFile, error) {
	client, cfg, _, err := s.dav(ctx)
	if err != nil {
		return nil, err
	}
	return client.list(ctx, cfg.Path)
}

// RunWebDAVBackup 立即创建、上传并执行保留清理。
func (s *Service) RunWebDAVBackup(ctx context.Context, target string) error {
	if err := s.begin(target); err != nil {
		return err
	}
	return s.runWebDAVBackupStarted(ctx, "")
}

// runWebDAVBackupStarted 执行已经取得任务锁的远程备份；slot 仅供定时任务成功后去重。
func (s *Service) runWebDAVBackupStarted(ctx context.Context, slot string) (err error) {
	var archive *Archive
	warning := ""
	defer func() {
		if archive != nil {
			s.finish(archive.Filename, archive.Size, warning, err)
			archive.Remove()
		} else {
			s.finish("", 0, warning, err)
		}
	}()
	client, cfg, password, err := s.dav(ctx)
	if err != nil {
		return err
	}
	archive, err = s.CreateArchive(ctx, password)
	if err != nil {
		return err
	}
	if err = client.put(ctx, cfg.Path, archive.Filename, archive.Path); err != nil {
		return err
	}
	// 上传成功即视为备份成功；保留清理失败只记录警告，避免管理员误判并重复上传。
	if cfg.Retention > 0 {
		files, listErr := client.list(ctx, cfg.Path)
		if listErr != nil {
			warning = "备份已上传，但读取远程列表失败：" + listErr.Error()
		} else {
			for i := cfg.Retention; i < len(files); i++ {
				if deleteErr := client.delete(ctx, cfg.Path, files[i].Name); deleteErr != nil {
					warning = "备份已上传，但清理旧备份失败：" + deleteErr.Error()
					break
				}
			}
		}
	}
	if warning != "" && s.Log != nil {
		s.Log.Warn("WebDAV 备份保留清理未完成", "warning", warning)
	}
	if slot != "" {
		_ = s.DB.SetSetting(ctx, "backup.webdav.lastSlot", slot)
	}
	return nil
}

// DownloadRemote 下载远程文件到本地临时文件。
func (s *Service) DownloadRemote(ctx context.Context, name string) (string, error) {
	client, cfg, _, err := s.dav(ctx)
	if err != nil {
		return "", err
	}
	root, err := os.MkdirTemp(s.DataDir, ".webdav-download-")
	if err != nil {
		return "", err
	}
	target := path.Join(root, name)
	if err := client.get(ctx, cfg.Path, name, target); err != nil {
		os.RemoveAll(root)
		return "", err
	}
	return target, nil
}

// DeleteRemote 删除远程备份。
func (s *Service) DeleteRemote(ctx context.Context, name string) error {
	client, cfg, _, err := s.dav(ctx)
	if err != nil {
		return err
	}
	return client.delete(ctx, cfg.Path, name)
}

// RestoreRemote 下载远程备份并暂存恢复。
func (s *Service) RestoreRemote(ctx context.Context, name, password string) (Manifest, error) {
	file, err := s.DownloadRemote(ctx, name)
	if err != nil {
		return Manifest{}, err
	}
	defer os.RemoveAll(path.Dir(file))
	if password == "" {
		_, _, configured, _ := s.webDAVSecrets(ctx)
		password = configured
	}
	return s.StageLocal(ctx, file, password, "WebDAV: "+name)
}

// OpenRemote 返回远程下载响应，供管理接口流式转发。
func (s *Service) OpenRemote(ctx context.Context, name string) (*http.Response, error) {
	client, cfg, _, err := s.dav(ctx)
	if err != nil {
		return nil, err
	}
	if !backupNamePattern.MatchString(name) {
		return nil, errors.New("远程文件名无效")
	}
	resp, err := client.request(ctx, http.MethodGet, client.target(cfg.Path, name), nil, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("下载失败: %s", resp.Status)
	}
	return resp, nil
}
