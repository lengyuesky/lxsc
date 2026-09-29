package db

import (
	"context"
	"database/sql"
	"errors"
	"lxsc/internal/admission"
	"sync"
	"testing"
	"time"
)

func holdStatisticsConnections(t *testing.T, d *DB) func() {
	t.Helper()
	var connections []*sql.Conn
	for range 2 {
		c, err := d.read.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, c)
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			for _, c := range connections {
				c.Close()
			}
		})
	}
	t.Cleanup(release)
	return release
}
func awaitListening(t *testing.T, d *DB, calls, waiters int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		d.listening.mu.Lock()
		n := len(d.listening.calls)
		w := 0
		for _, c := range d.listening.calls {
			w += c.waiters
		}
		d.listening.mu.Unlock()
		if n == calls && w == waiters {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("统计请求未进入预期状态：%+v", d.ListeningWorkload())
}
func TestListeningCoalescesAndKeepsIndependentResults(t *testing.T) {
	d, user, at := listeningFixture(t)
	release := holdStatisticsConnections(t, d)
	results := make(chan ListeningStats, 20)
	errs := make(chan error, 20)
	for range 20 {
		go func() { r, e := d.ListeningStatistics(context.Background(), user, 30, at); results <- r; errs <- e }()
	}
	awaitListening(t, d, 1, 20)
	release()
	var all []ListeningStats
	for range 20 {
		all = append(all, <-results)
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if d.ListeningQueries.Snapshot().Count != 1 {
		t.Fatal("重复统计未合并", d.ListeningQueries.Snapshot())
	}
	all[0].Daily[0].Day = "被调用者修改"
	if all[1].Daily[0].Day == all[0].Daily[0].Day {
		t.Fatal("不同请求不能共享可变结果切片")
	}
	if _, err := d.ListeningStatistics(context.Background(), user, 30, at); err != nil {
		t.Fatal(err)
	}
	if d.ListeningQueries.Snapshot().Count != 2 {
		t.Fatal("完成后不能复用旧结果")
	}
}
func TestListeningCancellationAndNewWaiter(t *testing.T) {
	d, user, at := listeningFixture(t)
	release := holdStatisticsConnections(t, d)
	first, cancel := context.WithCancel(context.Background())
	one := make(chan error, 1)
	two := make(chan error, 1)
	go func() { _, e := d.ListeningStatistics(first, user, 30, at); one <- e }()
	awaitListening(t, d, 1, 1)
	go func() { _, e := d.ListeningStatistics(context.Background(), user, 30, at); two <- e }()
	awaitListening(t, d, 1, 2)
	cancel()
	if err := <-one; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	awaitListening(t, d, 1, 1)
	release()
	if err := <-two; err != nil {
		t.Fatal("一个取消影响了其他等待者", err)
	}
	release = holdStatisticsConnections(t, d)
	last, cancelLast := context.WithCancel(context.Background())
	go func() { _, e := d.ListeningStatistics(last, user, 30, at); one <- e }()
	awaitListening(t, d, 1, 1)
	cancelLast()
	if err := <-one; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	awaitListening(t, d, 0, 0)
	go func() { _, e := d.ListeningStatistics(context.Background(), user, 30, at); two <- e }()
	awaitListening(t, d, 1, 1)
	release()
	if err := <-two; err != nil {
		t.Fatal("新等待者加入了已经取消的查询", err)
	}
}
func TestListeningWriteDateAndUserSeparateFlights(t *testing.T) {
	d, user, at := listeningFixture(t)
	release := holdStatisticsConnections(t, d)
	ctx := context.Background()
	results := make(chan error, 6)
	start := func(id int64, days int, at time.Time) {
		go func() { _, e := d.ListeningStatistics(ctx, id, days, at); results <- e }()
	}
	start(user, 30, at)
	awaitListening(t, d, 1, 1)
	p := ListeningProgress{UserID: user, SessionID: "version-session", TrackID: "tr-wy-1", StartedAt: at.Add(-time.Minute).UnixMilli(), Days: map[string]int64{at.In(ListeningZone).Format("2006-01-02"): 1000}}
	if err := d.SaveListeningProgress(ctx, p, ListeningTrack{ID: p.TrackID, Name: "歌曲"}, at); err != nil {
		t.Fatal(err)
	}
	start(user, 30, at)
	awaitListening(t, d, 2, 2)
	start(0, 30, at)
	start(user, 7, at)
	start(user, 30, at.AddDate(0, 0, 1))
	awaitListening(t, d, 5, 5)
	if err := d.UpdateUser(ctx, user, "新名字", "", false, "320k"); err != nil {
		t.Fatal(err)
	}
	start(user, 30, at)
	awaitListening(t, d, 6, 6)
	release()
	for range 6 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
}
func TestListeningQueueIsBoundedAndRecovers(t *testing.T) {
	d, _, at := listeningFixture(t)
	release := holdStatisticsConnections(t, d)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errs := make(chan error, 18)
	for i := int64(1); i <= 18; i++ {
		go func() { _, e := d.ListeningStatistics(ctx, i, 30, at); errs <- e }()
	}
	awaitListening(t, d, 18, 18)
	if _, err := d.ListeningStatistics(ctx, 999, 30, at); !errors.Is(err, admission.ErrBusy) {
		t.Fatal("超出统计队列应明确返回繁忙", err)
	}
	cancel()
	for range 18 {
		if err := <-errs; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	awaitListening(t, d, 0, 0)
	release()
	if _, err := d.ListeningStatistics(context.Background(), 1, 30, at); err != nil {
		t.Fatal("取消后统计未恢复", err)
	}
}
