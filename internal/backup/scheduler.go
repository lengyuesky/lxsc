package backup

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// StartScheduler 启动每日/每周 WebDAV 备份检查。
func (s *Service) StartScheduler(parent context.Context) {
	s.mu.Lock()
	if s.cancel != nil {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.mu.Unlock()
	s.scheduler.Add(1)
	go func() {
		defer s.scheduler.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		s.checkSchedule(ctx, time.Now())
		for {
			select {
			case now := <-ticker.C:
				s.checkSchedule(ctx, now)
			case <-ctx.Done():
				return
			}
		}
	}()
}

// StopScheduler 停止后台调度器。
func (s *Service) StopScheduler() {
	s.mu.Lock()
	cancel := s.cancel
	s.cancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.scheduler.Wait()
}

func (s *Service) checkSchedule(ctx context.Context, now time.Time) {
	cfg := s.GetWebDAVConfig(ctx)
	if !cfg.Auto || cfg.URL == "" {
		return
	}
	slot, ok := scheduleSlot(cfg, now)
	if !ok {
		return
	}
	retry, _ := strconv.ParseInt(s.DB.GetSetting(ctx, "backup.webdav.retryAt", "0"), 10, 64)
	if now.Unix() < retry {
		return
	}
	if s.DB.GetSetting(ctx, "backup.webdav.lastSlot", "") == slot {
		return
	}
	// 先取得任务锁，避免与手动备份冲突；忙碌时不消费本次计划槽位。
	if err := s.begin("scheduled"); err != nil {
		return
	}
	attempts, _ := strconv.Atoi(s.DB.GetSetting(ctx, "backup.webdav.attempts", "0"))
	if attempts < 0 || attempts > 12 {
		attempts = 0
	}
	attempts++
	delay := time.Duration(attempts) * 5 * time.Minute
	if delay > time.Hour {
		delay = time.Hour
	}
	if err := s.DB.SetSettings(ctx, map[string]string{"backup.webdav.retryAt": strconv.FormatInt(now.Add(delay).Unix(), 10), "backup.webdav.attempts": strconv.Itoa(attempts)}); err != nil {
		s.finish("", 0, "", err)
		return
	}
	s.scheduler.Add(1)
	go func() {
		defer s.scheduler.Done()
		taskCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
		defer cancel()
		if err := s.runWebDAVBackupStarted(taskCtx, slot); err != nil && s.Log != nil {
			s.Log.Error("WebDAV 定时备份失败", "err", err)
		}
	}()
}

// 补跑最近一次到期计划，错过多天也只生成一份当前快照。
func scheduleSlot(cfg WebDAVConfig, now time.Time) (string, bool) {
	clock, err := time.Parse("15:04", cfg.Time)
	if err != nil {
		return "", false
	}
	due := time.Date(now.Year(), now.Month(), now.Day(), clock.Hour(), clock.Minute(), 0, 0, now.Location())
	if due.After(now) {
		due = due.AddDate(0, 0, -1)
	}
	if cfg.Frequency == "weekly" {
		if cfg.Weekday < 0 || cfg.Weekday > 6 {
			return "", false
		}
		days := (int(due.Weekday()) - cfg.Weekday + 7) % 7
		due = due.AddDate(0, 0, -days)
		year, week := due.ISOWeek()
		return fmt.Sprintf("%04d-W%02d", year, week), true
	}
	return due.Format("2006-01-02"), true
}
