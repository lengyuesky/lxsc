package js

import (
	"context"
	"errors"
	"sync"
)

var ErrSourceChanged = errors.New("音源运行实例已变化，本次更新已取消")

func (m *SourceManager) sourceOperation(id int64) *sync.Mutex {
	m.operationMu.Lock()
	defer m.operationMu.Unlock()
	if m.operations == nil {
		m.operations = map[int64]*sync.Mutex{}
	}
	operation := m.operations[id]
	if operation == nil {
		operation = &sync.Mutex{}
		m.operations[id] = operation
	}
	return operation
}

// PreparedSource 独立验证候选脚本，在 Apply 成功前不参与播放取链。
type PreparedSource struct {
	mu       sync.Mutex
	manager  *SourceManager
	previous *loadedSource
	source   *loadedSource
	consumed bool
}

func (m *SourceManager) Prepare(ctx context.Context, id int64, script string) (*PreparedSource, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	previous := m.loaded[id]
	m.mu.RUnlock()
	ls, inited, err := m.newLoadedSource(id, script)
	if err != nil {
		return nil, err
	}
	if err := m.initializeSource(ctx, ls, script, inited); err != nil {
		ls.worker.Stop()
		return nil, err
	}
	return &PreparedSource{manager: m, previous: previous, source: ls}, nil
}

// Apply 在保存成功后切换实例；保存失败或原实例已变化时继续使用旧实例。
func (p *PreparedSource) Apply(ctx context.Context, save func() error) (*SourceStatus, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.consumed {
		return nil, ErrSourceChanged
	}
	m := p.manager
	operation := m.sourceOperation(p.source.id)
	operation.Lock()
	defer operation.Unlock()
	m.mu.Lock()
	if m.loaded[p.source.id] != p.previous {
		m.mu.Unlock()
		return nil, ErrSourceChanged
	}
	if err := ctx.Err(); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if err := save(); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	// 保存已完成后即使调用者取消，也必须安装对应实例，保持内存与数据库一致。
	m.attachSourceCalls(p.source)
	m.loaded[p.source.id] = p.source
	p.consumed = true
	status := m.statusOf(p.source)
	m.mu.Unlock()
	if p.previous != nil && p.previous.worker != nil {
		p.previous.worker.Stop()
	}
	return status, nil
}

func (p *PreparedSource) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.consumed {
		p.consumed = true
		p.source.worker.Stop()
	}
}
