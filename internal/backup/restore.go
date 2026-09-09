package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"lxsc/internal/urlcache"
)

// AppliedRestore 保存启动期恢复的回滚信息。
type AppliedRestore struct {
	dataDir   string
	rollback  string
	pending   PendingRestore
	restoreID string
	stage     string
	active    bool
}

type rollbackMeta struct {
	RestoreID string          `json:"restoreId"`
	Files     map[string]bool `json:"files"`
}

// ApplyPending 在数据库打开前应用已暂存恢复；调用方初始化成功后必须 Commit，失败时调用 Rollback。
func ApplyPending(dataDir string) (*AppliedRestore, error) {
	base := filepath.Join(dataDir, ".restore")
	pendingPath := filepath.Join(base, "pending.json")
	b, err := os.ReadFile(pendingPath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var pending PendingRestore
	if err := json.Unmarshal(b, &pending); err != nil {
		return nil, errors.New("待恢复任务信息已损坏")
	}
	restoreID := pending.ID
	stage := filepath.Join(base, "staging", pending.ID)
	if pending.ID == "" {
		restoreID = fmt.Sprintf("legacy-%d-%d", pending.CreatedAt, pending.BackupAt)
		stage = filepath.Join(base, "staging")
	}
	stagedDB := filepath.Join(stage, "lxsc.db")
	if _, err := os.Stat(stagedDB); err != nil {
		return nil, errors.New("待恢复数据库不存在")
	}
	if err := validateDatabase(stagedDB); err != nil {
		return nil, fmt.Errorf("待恢复数据库校验失败: %w", err)
	}
	rollback := filepath.Join(base, "rollback")
	if err := ensureRollback(dataDir, rollback, restoreID); err != nil {
		return nil, err
	}

	a := &AppliedRestore{dataDir: dataDir, rollback: rollback, pending: pending, restoreID: restoreID, stage: stage, active: true}
	if err := a.install(stage); err != nil {
		_ = a.Rollback()
		return nil, err
	}
	return a, nil
}

// ensureRollback 为同一个恢复任务只创建一次完整回滚副本，避免异常重启后覆盖原始数据。
func ensureRollback(dataDir, rollback, restoreID string) error {
	metaPath := filepath.Join(rollback, "complete.json")
	if b, err := os.ReadFile(metaPath); err == nil {
		var meta rollbackMeta
		if json.Unmarshal(b, &meta) == nil && meta.RestoreID == restoreID {
			return nil
		}
	}
	newDir := rollback + ".new"
	_ = os.RemoveAll(newDir)
	if err := os.MkdirAll(newDir, 0o700); err != nil {
		return err
	}
	meta := rollbackMeta{RestoreID: restoreID, Files: map[string]bool{}}
	for _, name := range []string{"lxsc.db", "lxsc.db-wal", "lxsc.db-shm", "config.yaml"} {
		source := filepath.Join(dataDir, name)
		if _, err := os.Stat(source); err == nil {
			if err := copyOptional(source, filepath.Join(newDir, name)); err != nil {
				return err
			}
			meta.Files[name] = true
		}
	}
	if info, err := os.Stat(filepath.Join(dataDir, "sources")); err == nil && info.IsDir() {
		if err := copyDirOptional(filepath.Join(dataDir, "sources"), filepath.Join(newDir, "sources")); err != nil {
			return err
		}
		meta.Files["sources"] = true
	}
	if err := syncTree(newDir); err != nil {
		return err
	}
	if err := writeJSONFile(filepath.Join(newDir, "complete.json"), meta); err != nil {
		return err
	}
	_ = os.RemoveAll(rollback)
	if err := os.Rename(newDir, rollback); err != nil {
		return err
	}
	return syncDir(filepath.Dir(rollback))
}

func (a *AppliedRestore) install(stage string) error {
	if err := urlcache.ResetFiles(a.dataDir); err != nil {
		return fmt.Errorf("清理恢复前的直链缓存失败: %w", err)
	}
	// 先完整写入临时文件，再原子替换主数据库；旧 WAL 会在替换后清理。
	if err := copyFileAtomic(filepath.Join(stage, "lxsc.db"), filepath.Join(a.dataDir, "lxsc.db")); err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(a.dataDir, "lxsc.db-wal"))
	_ = os.Remove(filepath.Join(a.dataDir, "lxsc.db-shm"))
	if _, err := os.Stat(filepath.Join(stage, "config.yaml")); err == nil {
		if err := copyFileAtomic(filepath.Join(stage, "config.yaml"), filepath.Join(a.dataDir, "config.yaml")); err != nil {
			return err
		}
	}
	return replaceDir(filepath.Join(stage, "sources"), filepath.Join(a.dataDir, "sources"))
}

// Rollback 还原恢复前的数据，并停用失败的待恢复任务，避免容器反复启动失败。
func (a *AppliedRestore) Rollback() error {
	if a == nil || !a.active {
		return nil
	}
	var meta rollbackMeta
	b, err := os.ReadFile(filepath.Join(a.rollback, "complete.json"))
	if err != nil || json.Unmarshal(b, &meta) != nil || meta.RestoreID != a.restoreID {
		return errors.New("恢复回滚副本不完整")
	}
	for _, name := range []string{"lxsc.db", "lxsc.db-wal", "lxsc.db-shm", "config.yaml"} {
		target := filepath.Join(a.dataDir, name)
		if meta.Files[name] {
			if err := copyFileAtomic(filepath.Join(a.rollback, name), target); err != nil {
				return err
			}
		} else {
			_ = os.Remove(target)
		}
	}
	if meta.Files["sources"] {
		if err := replaceDir(filepath.Join(a.rollback, "sources"), filepath.Join(a.dataDir, "sources")); err != nil {
			return err
		}
	} else {
		_ = os.RemoveAll(filepath.Join(a.dataDir, "sources"))
	}
	if err := urlcache.ResetFiles(a.dataDir); err != nil {
		return fmt.Errorf("清理回滚后的直链缓存失败: %w", err)
	}
	base := filepath.Join(a.dataDir, ".restore")
	_ = os.Remove(filepath.Join(base, "failed.json"))
	if err := os.Rename(filepath.Join(base, "pending.json"), filepath.Join(base, "failed.json")); err != nil && !os.IsNotExist(err) {
		return err
	}
	a.active = false
	return nil
}

// Commit 确认恢复成功。先移走 pending 标记，再清理暂存，保证清理中断后不会阻塞下次启动。
func (a *AppliedRestore) Commit() error {
	if a == nil || !a.active {
		return nil
	}
	base := filepath.Join(a.dataDir, ".restore")
	appliedPath := filepath.Join(base, "applied.json")
	_ = os.Remove(appliedPath)
	if err := os.Rename(filepath.Join(base, "pending.json"), appliedPath); err != nil {
		return err
	}
	a.active = false
	message := fmt.Sprintf("%s 已从 %s 的备份恢复（来源版本 %s）", time.Now().Format("2006-01-02 15:04:05"), time.Unix(a.pending.BackupAt, 0).Format("2006-01-02 15:04:05"), a.pending.SourceVersion)
	_ = writeFileAtomic(filepath.Join(base, "last-result.txt"), []byte(message), 0o600)
	_ = os.RemoveAll(a.stage)
	_ = os.Remove(appliedPath)
	return nil
}

// CancelPending 删除尚未生效的恢复任务。
func (s *Service) CancelPending() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy {
		return errors.New("备份任务执行期间不能取消恢复")
	}
	base := filepath.Dir(s.pendingPath())
	cancelled := filepath.Join(base, "cancelled.json")
	_ = os.Remove(cancelled)
	if err := os.Rename(s.pendingPath(), cancelled); err != nil && !os.IsNotExist(err) {
		return err
	}
	_ = os.RemoveAll(s.stagingDir())
	_ = os.Remove(cancelled)
	return nil
}

func copyFileAtomic(source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	tmp := target + ".restore-new"
	_ = os.Remove(tmp)
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := out.ReadFrom(in)
	if copyErr == nil {
		copyErr = out.Sync()
	}
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return closeErr
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return syncDir(filepath.Dir(target))
}

func replaceDir(source, target string) error {
	newDir := target + ".restore-new"
	oldDir := target + ".restore-old"
	_ = os.RemoveAll(newDir)
	_ = os.RemoveAll(oldDir)
	if err := os.MkdirAll(newDir, 0o700); err != nil {
		return err
	}
	if err := copyDirOptional(source, newDir); err != nil {
		return err
	}
	if _, err := os.Stat(target); err == nil {
		if err := os.Rename(target, oldDir); err != nil {
			return err
		}
	}
	if err := os.Rename(newDir, target); err != nil {
		if _, oldErr := os.Stat(oldDir); oldErr == nil {
			_ = os.Rename(oldDir, target)
		}
		return err
	}
	_ = os.RemoveAll(oldDir)
	return syncDir(filepath.Dir(target))
}

func writeJSONFile(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, b, 0o600)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".new"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil {
		_ = os.Remove(tmp)
		return writeErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return closeErr
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return syncDir(filepath.Dir(path))
}

// syncTree 确保暂存树中的文件内容和目录项在发布 pending 前落盘。
func syncTree(root string) error {
	var dirs []string
	if err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			dirs = append(dirs, path)
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		err = file.Sync()
		closeErr := file.Close()
		if err != nil {
			return err
		}
		return closeErr
	}); err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := syncDir(dirs[i]); err != nil {
			return err
		}
	}
	return nil
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
