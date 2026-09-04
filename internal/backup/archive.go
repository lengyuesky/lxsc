package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"filippo.io/age"
	"gopkg.in/yaml.v3"

	"lxsc/internal/secret"
)

const maxUnpackedSize int64 = 4 << 30

// Manifest 描述备份内容和兼容版本。
type Manifest struct {
	FormatVersion int               `json:"formatVersion"`
	SchemaVersion int               `json:"schemaVersion"`
	AppVersion    string            `json:"appVersion"`
	CreatedAt     int64             `json:"createdAt"`
	Encrypted     bool              `json:"encrypted"`
	Files         map[string]string `json:"files"`
	Stats         any               `json:"stats,omitempty"`
}

// Archive 是生成后需要由调用方关闭并删除的临时备份。
type Archive struct {
	Path     string
	Filename string
	Manifest Manifest
	Size     int64
}

// CreateArchive 创建一致、可选密码加密的完整迁移备份。
func (s *Service) CreateArchive(ctx context.Context, password string) (*Archive, error) {
	root, err := os.MkdirTemp(s.DataDir, ".backup-build-")
	if err != nil {
		return nil, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(root)
		}
	}()

	filesDir := filepath.Join(root, "files")
	if err := os.MkdirAll(filesDir, 0o700); err != nil {
		return nil, err
	}
	if err := s.DB.Snapshot(ctx, filepath.Join(filesDir, "database.sqlite")); err != nil {
		return nil, fmt.Errorf("创建数据库快照失败: %w", err)
	}
	if err := os.WriteFile(filepath.Join(filesDir, "effective-secret.key"), []byte(s.Secret.ExportKey()), 0o600); err != nil {
		return nil, err
	}
	if err := copyOptional(filepath.Join(s.DataDir, "config.yaml"), filepath.Join(filesDir, "config.yaml")); err != nil {
		return nil, err
	}
	if err := copyDirOptional(filepath.Join(s.DataDir, "sources"), filepath.Join(filesDir, "sources")); err != nil {
		return nil, err
	}

	now := time.Now()
	manifest := Manifest{FormatVersion: formatVersion, SchemaVersion: 1, AppVersion: s.Version, CreatedAt: now.Unix(), Encrypted: password != "", Files: map[string]string{}}
	stats, _ := s.DB.Statistics(ctx)
	manifest.Stats = stats
	if err := filepath.Walk(filesDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(filesDir, path)
		rel = filepath.ToSlash(rel)
		hash, err := fileHash(path)
		if err != nil {
			return err
		}
		manifest.Files[rel] = hash
		return nil
	}); err != nil {
		return nil, err
	}

	payload := filepath.Join(root, "payload.tar.gz")
	if err := writeTarGzip(payload, filesDir, manifest); err != nil {
		return nil, err
	}
	stamp := now.Format("20060102-150405") + fmt.Sprintf("-%03d", now.Nanosecond()/int(time.Millisecond))
	name := fmt.Sprintf("lxsc-%s-%s.lxsc-backup", stamp, safeVersion(s.Version))
	output := filepath.Join(root, name)
	if password == "" {
		if err := os.Rename(payload, output); err != nil {
			return nil, err
		}
	} else if err := encryptAge(payload, output, password); err != nil {
		return nil, err
	}
	info, err := os.Stat(output)
	if err != nil {
		return nil, err
	}
	cleanup = false
	return &Archive{Path: output, Filename: name, Manifest: manifest, Size: info.Size()}, nil
}

// Remove 删除备份临时目录。
func (a *Archive) Remove() {
	if a != nil {
		_ = os.RemoveAll(filepath.Dir(a.Path))
	}
}

func safeVersion(v string) string {
	v = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' {
			return r
		}
		return '-'
	}, v)
	if v == "" {
		return "dev"
	}
	return v
}

func writeTarGzip(target, root string, manifest Manifest) error {
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	mb, _ := json.MarshalIndent(manifest, "", "  ")
	if err := writeTarBytes(tw, "manifest.json", mb, 0o600); err != nil {
		return err
	}
	paths := make([]string, 0, len(manifest.Files))
	for p := range manifest.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, rel := range paths {
		path := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		h := &tar.Header{Name: "files/" + rel, Mode: int64(info.Mode().Perm()), Size: info.Size(), ModTime: info.ModTime(), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, in)
		_ = in.Close()
		if copyErr != nil {
			return copyErr
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func writeTarBytes(tw *tar.Writer, name string, b []byte, mode int64) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(b)), ModTime: time.Now(), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err := tw.Write(b)
	return err
}

func encryptAge(source, target, password string) error {
	recipient, err := age.NewScryptRecipient(password)
	if err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	writer, err := age.Encrypt(out, recipient)
	if err != nil {
		out.Close()
		return err
	}
	_, err = io.Copy(writer, in)
	closeErr := writer.Close()
	fileErr := out.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return fileErr
}

func decryptAge(source, target, password string) error {
	if password == "" {
		return errors.New("该备份已加密，请输入备份密码")
	}
	identity, err := age.NewScryptIdentity(password)
	if err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	reader, err := age.Decrypt(in, identity)
	if err != nil {
		return errors.New("备份密码错误或文件已损坏")
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	written, err := io.Copy(out, io.LimitReader(reader, maxUnpackedSize+1))
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err == nil && written > maxUnpackedSize {
		err = errors.New("加密备份超过大小限制")
	}
	return err
}

func fileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyOptional(source, target string) error {
	in, err := os.Open(source)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	return err
}

func copyDirOptional(source, target string) error {
	info, err := os.Stat(source)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return nil
	}
	return filepath.Walk(source, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, _ := filepath.Rel(source, path)
		dst := filepath.Join(target, rel)
		if info.IsDir() {
			return os.MkdirAll(dst, 0o700)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		return copyOptional(path, dst)
	})
}

// InspectArchive 校验备份并返回清单，不创建待恢复任务。
func (s *Service) InspectArchive(ctx context.Context, source, password string) (Manifest, error) {
	root, err := os.MkdirTemp(s.DataDir, ".backup-inspect-")
	if err != nil {
		return Manifest{}, err
	}
	defer os.RemoveAll(root)
	payload := source
	f, err := os.Open(source)
	if err != nil {
		return Manifest{}, err
	}
	prefix := make([]byte, 20)
	n, _ := f.Read(prefix)
	_ = f.Close()
	encrypted := strings.HasPrefix(string(prefix[:n]), "age-encryption.org/")
	if encrypted {
		payload = filepath.Join(root, "payload.tar.gz")
		if err := decryptAge(source, payload, password); err != nil {
			return Manifest{}, err
		}
	}
	extract := filepath.Join(root, "extract")
	manifest, err := extractAndVerify(payload, extract)
	if err != nil {
		return Manifest{}, err
	}
	manifest.Encrypted = encrypted
	if manifest.FormatVersion != formatVersion || manifest.SchemaVersion > 1 {
		return Manifest{}, errors.New("不支持的备份格式或数据库版本")
	}
	if err := validateDatabase(filepath.Join(extract, "files", "database.sqlite")); err != nil {
		return Manifest{}, err
	}
	key, err := os.ReadFile(filepath.Join(extract, "files", "effective-secret.key"))
	if err != nil || strings.TrimSpace(string(key)) == "" {
		return Manifest{}, errors.New("备份缺少有效迁移密钥")
	}
	select {
	case <-ctx.Done():
		return Manifest{}, ctx.Err()
	default:
	}
	return manifest, nil
}

// InspectAndStage 校验备份并暂存为下次启动恢复内容。
func (s *Service) InspectAndStage(ctx context.Context, source, password, sourceName string) (Manifest, error) {
	root, err := os.MkdirTemp(s.DataDir, ".backup-import-")
	if err != nil {
		return Manifest{}, err
	}
	defer os.RemoveAll(root)
	payload := source
	f, err := os.Open(source)
	if err != nil {
		return Manifest{}, err
	}
	prefix := make([]byte, 20)
	n, _ := f.Read(prefix)
	_ = f.Close()
	encrypted := strings.HasPrefix(string(prefix[:n]), "age-encryption.org/")
	if encrypted {
		payload = filepath.Join(root, "payload.tar.gz")
		if err := decryptAge(source, payload, password); err != nil {
			return Manifest{}, err
		}
	}
	extract := filepath.Join(root, "extract")
	manifest, err := extractAndVerify(payload, extract)
	if err != nil {
		return Manifest{}, err
	}
	manifest.Encrypted = encrypted
	if manifest.FormatVersion != formatVersion || manifest.SchemaVersion > 1 {
		return Manifest{}, errors.New("不支持的备份格式或数据库版本")
	}
	dbPath := filepath.Join(extract, "files", "database.sqlite")
	keyBytes, err := os.ReadFile(filepath.Join(extract, "files", "effective-secret.key"))
	if err != nil {
		return Manifest{}, errors.New("备份缺少迁移密钥")
	}
	if err := validateDatabase(dbPath); err != nil {
		return Manifest{}, err
	}
	if err := s.reencryptDatabase(ctx, dbPath, strings.TrimSpace(string(keyBytes))); err != nil {
		return Manifest{}, err
	}

	pending := PendingRestore{ID: secret.RandomToken(12), CreatedAt: time.Now().Unix(), BackupAt: manifest.CreatedAt, SourceVersion: manifest.AppVersion, Encrypted: encrypted, Source: sourceName}
	stage := s.stagingDir(pending.ID)
	stageNew := stage + ".new"
	_ = os.RemoveAll(stageNew)
	defer os.RemoveAll(stageNew)
	if err := os.MkdirAll(stageNew, 0o700); err != nil {
		return Manifest{}, err
	}
	if err := copyOptional(dbPath, filepath.Join(stageNew, "lxsc.db")); err != nil {
		return Manifest{}, err
	}
	if err := s.mergeConfig(filepath.Join(extract, "files", "config.yaml"), filepath.Join(stageNew, "config.yaml")); err != nil {
		return Manifest{}, err
	}
	if err := copyDirOptional(filepath.Join(extract, "files", "sources"), filepath.Join(stageNew, "sources")); err != nil {
		return Manifest{}, err
	}
	if err := syncTree(stageNew); err != nil {
		return Manifest{}, err
	}
	if err := os.MkdirAll(s.stagingRoot(), 0o700); err != nil {
		return Manifest{}, err
	}
	if err := os.Rename(stageNew, stage); err != nil {
		return Manifest{}, err
	}
	if err := syncDir(s.stagingRoot()); err != nil {
		return Manifest{}, err
	}
	if err := writeJSONFile(s.pendingPath(), pending); err != nil {
		return Manifest{}, err
	}
	// pending 发布成功后，旧恢复任务的不可变暂存目录才可以清理。
	if entries, err := os.ReadDir(s.stagingRoot()); err == nil {
		for _, entry := range entries {
			if entry.IsDir() && entry.Name() != pending.ID {
				_ = os.RemoveAll(s.stagingDir(entry.Name()))
			}
		}
	}
	return manifest, nil
}

func extractAndVerify(payload, target string) (Manifest, error) {
	f, err := os.Open(payload)
	if err != nil {
		return Manifest{}, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return Manifest{}, errors.New("不是有效的 lxsc 备份文件")
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var total int64
	files := 0
	var extractedFiles []string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Manifest{}, err
		}
		name := filepath.ToSlash(filepath.Clean(h.Name))
		if strings.HasPrefix(name, "/") || name == ".." || strings.HasPrefix(name, "../") {
			return Manifest{}, errors.New("备份包含非法路径")
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		if name != "manifest.json" && !strings.HasPrefix(name, "files/") {
			return Manifest{}, errors.New("备份包含清单外的顶层文件")
		}
		if strings.HasPrefix(name, "files/") {
			extractedFiles = append(extractedFiles, strings.TrimPrefix(name, "files/"))
		}
		files++
		if files > 10000 || h.Size < 0 {
			return Manifest{}, errors.New("备份文件数量或大小超限")
		}
		if h.Size > maxUnpackedSize-total {
			return Manifest{}, errors.New("备份解包大小超过限制")
		}
		total += h.Size
		dst := filepath.Join(target, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return Manifest{}, err
		}
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return Manifest{}, err
		}
		_, copyErr := io.CopyN(out, tr, h.Size)
		closeErr := out.Close()
		if copyErr != nil {
			return Manifest{}, copyErr
		}
		if closeErr != nil {
			return Manifest{}, closeErr
		}
	}
	var manifest Manifest
	b, err := os.ReadFile(filepath.Join(target, "manifest.json"))
	if err != nil || json.Unmarshal(b, &manifest) != nil {
		return Manifest{}, errors.New("备份清单无效")
	}
	if len(manifest.Files) == 0 {
		return Manifest{}, errors.New("备份清单为空")
	}
	if len(extractedFiles) != len(manifest.Files) {
		return Manifest{}, errors.New("备份内容与清单数量不一致")
	}
	for _, name := range extractedFiles {
		if _, ok := manifest.Files[name]; !ok {
			return Manifest{}, fmt.Errorf("备份包含未登记文件: %s", name)
		}
	}
	for name, want := range manifest.Files {
		if strings.Contains(name, "..") || strings.HasPrefix(name, "/") {
			return Manifest{}, errors.New("备份清单包含非法路径")
		}
		got, err := fileHash(filepath.Join(target, "files", filepath.FromSlash(name)))
		if err != nil || got != want {
			return Manifest{}, fmt.Errorf("备份文件校验失败: %s", name)
		}
	}
	return manifest, nil
}

func validateDatabase(path string) error {
	database, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
	if err != nil {
		return err
	}
	defer database.Close()
	var result string
	if err := database.QueryRow(`PRAGMA integrity_check`).Scan(&result); err != nil || result != "ok" {
		return errors.New("SQLite 完整性检查失败")
	}
	for _, table := range []string{"users", "settings", "sources", "tracks", "playlists", "playlist_tracks"} {
		var n int
		if err := database.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil || n != 1 {
			return fmt.Errorf("备份数据库缺少核心表: %s", table)
		}
	}
	return nil
}

func (s *Service) reencryptDatabase(ctx context.Context, path, sourceKey string) error {
	sourceKey = strings.TrimSpace(sourceKey)
	if sourceKey == "" {
		return errors.New("迁移密钥为空")
	}
	sourceBox, err := secret.Load("", sourceKey)
	if err != nil {
		return errors.New("迁移密钥无效")
	}
	database, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		return err
	}
	defer database.Close()
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id, password_enc FROM users`)
	if err != nil {
		return err
	}
	type pair struct {
		id  int64
		enc string
	}
	var users []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.id, &p.enc); err != nil {
			rows.Close()
			return err
		}
		users = append(users, p)
	}
	rows.Close()
	for _, p := range users {
		plain, err := sourceBox.Decrypt(p.enc)
		if err != nil {
			return fmt.Errorf("用户 %d 的密码无法使用备份密钥解密", p.id)
		}
		enc, err := s.Secret.Encrypt(plain)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET password_enc=? WHERE id=?`, enc, p.id); err != nil {
			return err
		}
	}
	for _, key := range []string{"backup.webdav.password", "backup.webdav.backupPassword"} {
		var enc string
		err := tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&enc)
		if errors.Is(err, sql.ErrNoRows) || enc == "" {
			continue
		}
		if err != nil {
			return err
		}
		plain, err := sourceBox.Decrypt(enc)
		if err != nil {
			return errors.New("备份中的 WebDAV 凭据无法解密")
		}
		newEnc, err := s.Secret.Encrypt(plain)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE settings SET value=? WHERE key=?`, newEnc, key); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Service) mergeConfig(incoming, target string) error {
	merged := map[string]any{}
	if b, err := os.ReadFile(incoming); err == nil {
		_ = yaml.Unmarshal(b, &merged)
	}
	current := map[string]any{}
	if b, err := os.ReadFile(filepath.Join(s.DataDir, "config.yaml")); err == nil {
		_ = yaml.Unmarshal(b, &current)
	}
	for _, key := range []string{"listen", "data_dir", "secret_key", "admin_user", "admin_password"} {
		if value, ok := current[key]; ok {
			merged[key] = value
		} else {
			delete(merged, key)
		}
	}
	if len(merged) == 0 {
		return nil
	}
	b, err := yaml.Marshal(merged)
	if err != nil {
		return err
	}
	return os.WriteFile(target, b, 0o600)
}
