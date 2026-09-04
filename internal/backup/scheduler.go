package backup

import (
	"context"
	"fmt"
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
	clock, err := time.Parse("15:04", cfg.Time)
	if err != nil || now.Hour() != clock.Hour() || now.Minute() != clock.Minute() {
		return
	}
	if cfg.Frequency == "weekly" && int(now.Weekday()) != cfg.Weekday {
		return
	}
	slot := now.Format("2006-01-02")
	if cfg.Frequency == "weekly" {
		year, week := now.ISOWeek()
		slot = fmt.Sprintf("%04d-W%02d", year, week)
	}
	if s.DB.GetSetting(ctx, "backup.webdav.lastSlot", "") == slot {
		return
	}
	// 先取得任务锁，避免与手动备份冲突；忙碌时不消费本次计划槽位。
	if err := s.begin("scheduled"); err != nil {
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
