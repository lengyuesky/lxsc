package music

import (
	"testing"
	"time"
)

// 所在平台连续多首歌确认全部失败才算整体持续失败；一次成功、过期或音源变更都清除结论。
func TestHomePlatformFailingNeedsConsecutiveConfirmedFailures(t *testing.T) {
	c := newStabilityCatalog(t)
	for i := 1; i < platformFailureStreak; i++ {
		c.RecordHomePlatform("tx", true, false)
	}
	if c.HomePlatformFailing("tx", true) {
		t.Fatal("连续失败次数不足时不应判定整体失败")
	}
	c.RecordHomePlatform("tx", true, false)
	if !c.HomePlatformFailing("tx", true) {
		t.Fatal("连续确认失败达到阈值后应判定整体失败")
	}
	if c.HomePlatformFailing("tx", false) || c.HomePlatformFailing("wy", true) {
		t.Fatal("结论只适用于同一平台与验证条件")
	}

	c.RecordHomePlatform("tx", true, true)
	if c.HomePlatformFailing("tx", true) {
		t.Fatal("一次成功应清零")
	}

	for range platformFailureStreak {
		c.RecordHomePlatform("tx", true, false)
	}
	c.platformMu.Lock()
	failure := c.platformFailures[platformKey{"tx", true}]
	failure.at = time.Now().Add(-platformFailureTTL)
	c.platformFailures[platformKey{"tx", true}] = failure
	c.platformMu.Unlock()
	if c.HomePlatformFailing("tx", true) {
		t.Fatal("最近一次确认失败过期后应恢复先试所在平台")
	}
	c.RecordHomePlatform("tx", true, false)
	if !c.HomePlatformFailing("tx", true) {
		t.Fatal("过期后再次确认失败应立即恢复判定，连续计数不因过期清零")
	}

	c.InvalidateURLs()
	if c.HomePlatformFailing("tx", true) {
		t.Fatal("音源变更后旧结论应作废")
	}
}
