// Package config 读取 config.yaml 并允许 LXSC_* 环境变量覆盖
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"lxsc/internal/httpguard"
)

// Config 启动期配置（运行时可改项存放于 SQLite settings 表）
type Config struct {
	Listen         string   `yaml:"listen"`     // 监听地址，默认 :8080
	DataDir        string   `yaml:"data_dir"`   // 数据目录：数据库、音源脚本
	AdminUser      string   `yaml:"admin_user"` // 首次启动创建的管理员
	AdminPassword  string   `yaml:"admin_password"`
	Proxy          string   `yaml:"proxy"`           // 上游代理 http(s)://或 socks5://
	SDKWorkers     int      `yaml:"sdk_workers"`     // SDK 工作线程数
	LogLevel       string   `yaml:"log_level"`       // debug/info/warn/error
	SecretKey      string   `yaml:"secret_key"`      // 口令加密密钥，留空则首次生成并写入 data_dir/secret.key
	BaseURL        string   `yaml:"base_url"`        // 对外访问地址（可选，用于日志/提示）
	TrustProxy     bool     `yaml:"trust_proxy"`     // 仅对明确列出的代理信任转发头
	TrustedProxies []string `yaml:"trusted_proxies"` // 可信代理 CIDR 列表
}

// Default 返回默认配置
func Default() Config {
	return Config{
		Listen:        ":8080",
		DataDir:       "./data",
		AdminUser:     "admin",
		AdminPassword: "",
		SDKWorkers:    2,
		LogLevel:      "info",
	}
}

// Load 默认读取数据目录配置，再应用环境变量；无效文件不能静默退回默认值。
func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		dir := cfg.DataDir
		if value := strings.TrimSpace(os.Getenv("LXSC_DATA_DIR")); value != "" {
			dir = value
		}
		path = filepath.Join(dir, "config.yaml")
	}
	if path != "" {
		b, err := os.ReadFile(path)
		if err == nil {
			if err := yaml.Unmarshal(b, &cfg); err != nil {
				return cfg, fmt.Errorf("解析配置 %s: %w", path, err)
			}
		} else if !os.IsNotExist(err) {
			return cfg, err
		}
	}
	applyEnv(&cfg)
	if _, err := httpguard.TrustedPrefixes(cfg.TrustedProxies); err != nil {
		return cfg, err
	}
	if cfg.TrustProxy && len(cfg.TrustedProxies) == 0 {
		return cfg, fmt.Errorf("启用 trust_proxy 必须配置 trusted_proxies 可信代理网段")
	}
	if cfg.SDKWorkers <= 0 {
		cfg.SDKWorkers = 2
	}
	abs, err := filepath.Abs(cfg.DataDir)
	if err == nil {
		cfg.DataDir = abs
	}
	return cfg, nil
}

func applyEnv(cfg *Config) {
	get := func(k string) (string, bool) {
		v, ok := os.LookupEnv("LXSC_" + k)
		return strings.TrimSpace(v), ok
	}
	if v, ok := get("LISTEN"); ok && v != "" {
		cfg.Listen = v
	}
	if v, ok := get("PORT"); ok && v != "" {
		cfg.Listen = ":" + strings.TrimPrefix(v, ":")
	}
	if v, ok := get("DATA_DIR"); ok && v != "" {
		cfg.DataDir = v
	}
	if v, ok := get("ADMIN_USER"); ok && v != "" {
		cfg.AdminUser = v
	}
	if v, ok := get("ADMIN_PASSWORD"); ok && v != "" {
		cfg.AdminPassword = v
	}
	if v, ok := get("PROXY"); ok {
		cfg.Proxy = v
	}
	if v, ok := get("SDK_WORKERS"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.SDKWorkers = n
		}
	}
	if v, ok := get("LOG_LEVEL"); ok && v != "" {
		cfg.LogLevel = v
	}
	if v, ok := get("SECRET_KEY"); ok && v != "" {
		cfg.SecretKey = v
	}
	if v, ok := get("BASE_URL"); ok {
		cfg.BaseURL = v
	}
	if v, ok := get("TRUST_PROXY"); ok {
		cfg.TrustProxy = v == "1" || strings.EqualFold(v, "true")
	}
	if v, ok := get("TRUSTED_PROXIES"); ok {
		cfg.TrustedProxies = nil
		if v != "" {
			cfg.TrustedProxies = strings.Split(v, ",")
		}
	}
}
