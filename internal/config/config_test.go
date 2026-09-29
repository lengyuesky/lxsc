package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultConfigAndProxyValidation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LXSC_DATA_DIR", dir)
	t.Setenv("LXSC_ADMIN_PASSWORD", "")
	t.Setenv("LXSC_TRUST_PROXY", "false")
	t.Setenv("LXSC_TRUSTED_PROXIES", "")
	cfg, err := Load("")
	if err != nil || cfg.AdminPassword != "" {
		t.Fatal("不能回退到默认密码", cfg.AdminPassword, err)
	}
	t.Setenv("LXSC_TRUST_PROXY", "true")
	if _, err := Load(""); err == nil {
		t.Fatal("不能隐式信任所有代理")
	}
	t.Setenv("LXSC_TRUSTED_PROXIES", "10.0.0.0/24,::1/128")
	if _, err := Load(""); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("listen: [bad yaml"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(""); err == nil {
		t.Fatal("配置错误不能被忽略")
	}
}

func TestEnvironmentTrustUsesLocalProxyRanges(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LXSC_DATA_DIR", dir)
	t.Setenv("LXSC_TRUST_PROXY", "true")
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("trusted_proxies: [127.0.0.1/32]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load("")
	if err != nil || len(cfg.TrustedProxies) != 1 {
		t.Fatal("必须先读取文件再校验环境配置", err)
	}
}
