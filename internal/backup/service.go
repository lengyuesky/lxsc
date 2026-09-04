// Package backup 提供完整迁移备份、恢复暂存、WebDAV 与定时任务。
package backup

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"lxsc/internal/db"
	"lxsc/internal/secret"
)

const (
	formatVersion = 1
	defaultDir    = "lxsc-backups"
)

// WebDAVConfig 是管理员可见的远程备份配置，密码只以是否已配置的形式返回。
type WebDAVConfig struct {
	URL                      string `json:"url"`
	Username                 string `json:"username"`
	Password                 string `json:"password,omitempty"`
	PasswordConfigured       bool   `json:"passwordConfigured"`
	ClearPassword            bool   `json:"clearPassword,omitempty"`
	Path                     string `json:"path"`
	Auto                     bool   `json:"auto"`
	Frequency                string `json:"frequency"`
	Time                     string `json:"time"`
	Weekday                  int    `json:"weekday"`
	Retention                int    `json:"retention"`
	BackupPassword           string `json:"backupPassword,omitempty"`
	BackupPasswordConfigured bool   `json:"backupPasswordConfigured"`
	ClearBackupPassword      bool   `json:"clearBackupPassword,omitempty"`
	Timezone                 string `json:"timezone"`
}

// JobStatus 记录最近一次备份任务。
type JobStatus struct {
	StartedAt  int64  `json:"startedAt,omitempty"`
	FinishedAt int64  `json:"finishedAt,omitempty"`
	Target     string `json:"target,omitempty"`
	Filename   string `json:"filename,omitempty"`
	Size       int64  `json:"size,omitempty"`
	Warning    string `json:"warning,omitempty"`
	Error      string `json:"error,omitempty"`
}

// PendingRestore 是已经校验、等待重启生效的恢复任务。
type PendingRestore struct {
	ID            string `json:"id"`
	CreatedAt     int64  `json:"createdAt"`
	BackupAt      int64  `json:"backupAt"`
	SourceVersion string `json:"sourceVersion"`
	Encrypted     bool   `json:"encrypted"`
	Source        string `json:"source"`
}

// Status 是概览与备份页共用的状态。
type Status struct {
	Busy        bool            `json:"busy"`
	Last        JobStatus       `json:"last"`
	Pending     *PendingRestore `json:"pending,omitempty"`
	LastRestore string          `json:"lastRestore,omitempty"`
}

// Service 管理备份生命周期。
type Service struct {
	DB      *db.DB
	Secret  *secret.Box
	DataDir string
	Version string
	HTTP    *http.Client
	Log     *slog.Logger

	mu        sync.Mutex
	busy      bool
	last      JobStatus
	cancel    context.CancelFunc
	scheduler sync.WaitGroup
}

// New 创建备份服务。
func New(database *db.DB, box *secret.Box, dataDir, version string, client *http.Client, log *slog.Logger) *Service {
	if client == nil {
		client = http.DefaultClient
	}
	s := &Service{DB: database, Secret: box, DataDir: dataDir, Version: version, HTTP: client, Log: log}
	s.loadState()
	return s
}

func (s *Service) statePath() string   { return filepath.Join(s.DataDir, "backup-state.json") }
func (s *Service) pendingPath() string { return filepath.Join(s.DataDir, ".restore", "pending.json") }
func (s *Service) stagingRoot() string { return filepath.Join(s.DataDir, ".restore", "staging") }
func (s *Service) stagingDir(id ...string) string {
	if len(id) > 0 && id[0] != "" {
		return filepath.Join(s.stagingRoot(), id[0])
	}
	return s.stagingRoot()
}

func (s *Service) loadState() {
	b, err := os.ReadFile(s.statePath())
	if err == nil {
		_ = json.Unmarshal(b, &s.last)
	}
}

func (s *Service) saveState() {
	b, _ := json.MarshalIndent(s.last, "", "  ")
	_ = os.WriteFile(s.statePath(), b, 0o600)
}

func (s *Service) begin(target string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy {
		return errors.New("已有备份任务正在执行")
	}
	s.busy = true
	s.last = JobStatus{StartedAt: time.Now().Unix(), Target: target}
	s.saveState()
	return nil
}

func (s *Service) finish(filename string, size int64, warning string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.busy = false
	s.last.FinishedAt = time.Now().Unix()
	s.last.Filename = filename
	s.last.Size = size
	s.last.Warning = warning
	if err != nil {
		s.last.Error = err.Error()
	}
	s.saveState()
}

// Status 返回当前任务与待恢复信息。
func (s *Service) Status() Status {
	s.mu.Lock()
	out := Status{Busy: s.busy, Last: s.last}
	s.mu.Unlock()
	if b, err := os.ReadFile(s.pendingPath()); err == nil {
		var p PendingRestore
		if json.Unmarshal(b, &p) == nil {
			out.Pending = &p
		}
	}
	if b, err := os.ReadFile(filepath.Join(s.DataDir, ".restore", "last-result.txt")); err == nil {
		out.LastRestore = strings.TrimSpace(string(b))
	}
	return out
}

func settingBool(v string) bool { return v == "1" || strings.EqualFold(v, "true") }

// GetWebDAVConfig 读取 WebDAV 配置。
func (s *Service) GetWebDAVConfig(ctx context.Context) WebDAVConfig {
	get := func(k, def string) string { return s.DB.GetSetting(ctx, "backup.webdav."+k, def) }
	retention, _ := strconv.Atoi(get("retention", "3"))
	weekday, _ := strconv.Atoi(get("weekday", "1"))
	if retention < 0 {
		retention = 3
	}
	cfg := WebDAVConfig{
		URL: get("url", ""), Username: get("username", ""), Path: get("path", defaultDir),
		Auto: settingBool(get("auto", "false")), Frequency: get("frequency", "daily"), Time: get("time", "03:00"),
		Weekday: weekday, Retention: retention,
		PasswordConfigured: get("password", "") != "", BackupPasswordConfigured: get("backupPassword", "") != "",
		Timezone: time.Now().Location().String(),
	}
	return cfg
}

// SaveWebDAVConfig 校验并保存配置，空密码表示保留旧值。
func (s *Service) SaveWebDAVConfig(ctx context.Context, cfg WebDAVConfig) (WebDAVConfig, error) {
	cfg.URL = strings.TrimSpace(cfg.URL)
	cfg.Username = strings.TrimSpace(cfg.Username)
	cfg.Path = strings.Trim(strings.TrimSpace(cfg.Path), "/")
	if cfg.Path == "" {
		cfg.Path = defaultDir
	}
	if cfg.URL != "" {
		parsed, err := url.Parse(cfg.URL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return WebDAVConfig{}, errors.New("WebDAV 地址必须是有效的 http:// 或 https:// URL")
		}
		if parsed.User != nil {
			return WebDAVConfig{}, errors.New("WebDAV 地址不能包含用户名或密码，请使用专用凭据字段")
		}
		if parsed.Fragment != "" {
			return WebDAVConfig{}, errors.New("WebDAV 地址不能包含片段标识")
		}
	}
	if cfg.Frequency != "daily" && cfg.Frequency != "weekly" {
		return WebDAVConfig{}, errors.New("备份周期必须为每日或每周")
	}
	if _, err := time.Parse("15:04", cfg.Time); err != nil {
		return WebDAVConfig{}, errors.New("执行时间格式应为 HH:MM")
	}
	if cfg.Weekday < 0 || cfg.Weekday > 6 {
		return WebDAVConfig{}, errors.New("星期参数无效")
	}
	if cfg.Retention < 0 || cfg.Retention > 1000 {
		return WebDAVConfig{}, errors.New("保留数量必须为 0–1000")
	}
	set := func(k, v string) error { return s.DB.SetSetting(ctx, "backup.webdav."+k, v) }
	values := map[string]string{
		"url": cfg.URL, "username": cfg.Username, "path": cfg.Path, "auto": strconv.FormatBool(cfg.Auto),
		"frequency": cfg.Frequency, "time": cfg.Time, "weekday": strconv.Itoa(cfg.Weekday), "retention": strconv.Itoa(cfg.Retention),
	}
	for k, v := range values {
		if err := set(k, v); err != nil {
			return WebDAVConfig{}, err
		}
	}
	if cfg.ClearPassword {
		if err := set("password", ""); err != nil {
			return WebDAVConfig{}, err
		}
	} else if cfg.Password != "" {
		enc, err := s.Secret.Encrypt(cfg.Password)
		if err != nil {
			return WebDAVConfig{}, err
		}
		if err := set("password", enc); err != nil {
			return WebDAVConfig{}, err
		}
	}
	if cfg.ClearBackupPassword {
		if err := set("backupPassword", ""); err != nil {
			return WebDAVConfig{}, err
		}
	} else if cfg.BackupPassword != "" {
		enc, err := s.Secret.Encrypt(cfg.BackupPassword)
		if err != nil {
			return WebDAVConfig{}, err
		}
		if err := set("backupPassword", enc); err != nil {
			return WebDAVConfig{}, err
		}
	}
	return s.GetWebDAVConfig(ctx), nil
}

// ExportLocal 生成本地下载备份并记录任务状态。
func (s *Service) ExportLocal(ctx context.Context, password string) (archive *Archive, err error) {
	if err = s.begin("local"); err != nil {
		return nil, err
	}
	defer func() {
		if archive != nil {
			s.finish(archive.Filename, archive.Size, "", err)
		} else {
			s.finish("", 0, "", err)
		}
	}()
	archive, err = s.CreateArchive(ctx, password)
	return archive, err
}

// StageLocal 校验上传文件并暂存恢复。
func (s *Service) StageLocal(ctx context.Context, source, password, sourceName string) (manifest Manifest, err error) {
	if err = s.begin("restore"); err != nil {
		return manifest, err
	}
	defer func() { s.finish(sourceName, 0, "", err) }()
	return s.InspectAndStage(ctx, source, password, sourceName)
}

func (s *Service) webDAVSecrets(ctx context.Context) (WebDAVConfig, string, string, error) {
	cfg := s.GetWebDAVConfig(ctx)
	decrypt := func(k string) (string, error) {
		enc := s.DB.GetSetting(ctx, "backup.webdav."+k, "")
		if enc == "" {
			return "", nil
		}
		return s.Secret.Decrypt(enc)
	}
	password, err := decrypt("password")
	if err != nil {
		return cfg, "", "", errors.New("WebDAV 密码无法解密")
	}
	backupPassword, err := decrypt("backupPassword")
	if err != nil {
		return cfg, "", "", errors.New("备份密码无法解密")
	}
	return cfg, password, backupPassword, nil
}
