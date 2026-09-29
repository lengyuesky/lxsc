// Package admission 限制执行中与排队任务，取消后立即归还名额。
package admission

import (
	"context"
	"errors"
	"sync"
)

var ErrBusy = errors.New("服务繁忙，请稍后重试")

type usage struct{ active, queued int }
type Gate struct {
	mu                                           sync.Mutex
	limit, queueLimit, userLimit, userQueueLimit int
	active                                       int
	queue                                        []*Permit
	users                                        map[int64]*usage
	rejected                                     uint64
}
type Permit struct {
	gate  *Gate
	user  int64
	ready chan struct{}
	state int // 0 为排队，1 为执行中，2 为已释放。
}
type Stats struct {
	Active     int    `json:"active"`
	Queued     int    `json:"queued"`
	Limit      int    `json:"limit"`
	QueueLimit int    `json:"queueLimit"`
	Rejected   uint64 `json:"rejected"`
}

func New(limit, queueLimit, userLimit, userQueueLimit int) *Gate {
	if limit < 1 || queueLimit < 0 || userLimit < 0 || userQueueLimit < 0 {
		panic("无效的并发限制")
	}
	return &Gate{limit: limit, queueLimit: queueLimit, userLimit: userLimit, userQueueLimit: userQueueLimit, users: make(map[int64]*usage)}
}

// Reserve 不阻塞，可在共享请求锁内调用；只有新的上游任务才占用一个名额。
func (g *Gate) Reserve(user int64) (*Permit, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	u := g.users[user]
	if u == nil {
		u = &usage{}
	}
	canRun := g.active < g.limit && (g.userLimit == 0 || u.active < g.userLimit)
	if !canRun && (len(g.queue) >= g.queueLimit || (g.userLimit > 0 && u.queued >= g.userQueueLimit)) {
		g.rejected++
		return nil, ErrBusy
	}
	g.users[user] = u
	p := &Permit{gate: g, user: user, ready: make(chan struct{})}
	if canRun {
		p.state = 1
		g.active++
		u.active++
		close(p.ready)
	} else {
		u.queued++
		g.queue = append(g.queue, p)
	}
	return p, nil
}

func (p *Permit) Wait(ctx context.Context) error {
	select {
	case <-p.ready:
		if err := ctx.Err(); err != nil {
			p.Release()
			return err
		}
		return nil
	case <-ctx.Done():
		p.Release()
		return ctx.Err()
	}
}

func (g *Gate) Acquire(ctx context.Context, user int64) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p, err := g.Reserve(user)
	if err != nil {
		return nil, err
	}
	if err := p.Wait(ctx); err != nil {
		return nil, err
	}
	return p.Release, nil
}

// Release 幂等；分配时跳过达到个人上限的用户，避免阻挡其他用户。
func (p *Permit) Release() {
	g := p.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	if p.state == 2 {
		return
	}
	u := g.users[p.user]
	if p.state == 1 {
		g.active--
		u.active--
	} else {
		u.queued--
		for i, queued := range g.queue {
			if queued == p {
				g.queue = append(g.queue[:i], g.queue[i+1:]...)
				break
			}
		}
	}
	p.state = 2
	if u.active == 0 && u.queued == 0 {
		delete(g.users, p.user)
	}
	for i := 0; i < len(g.queue) && g.active < g.limit; {
		next := g.queue[i]
		owner := g.users[next.user]
		if g.userLimit > 0 && owner.active >= g.userLimit {
			i++
			continue
		}
		g.queue = append(g.queue[:i], g.queue[i+1:]...)
		owner.queued--
		owner.active++
		g.active++
		next.state = 1
		close(next.ready)
	}
}

func (g *Gate) Stats() Stats {
	g.mu.Lock()
	defer g.mu.Unlock()
	return Stats{g.active, len(g.queue), g.limit, g.queueLimit, g.rejected}
}
