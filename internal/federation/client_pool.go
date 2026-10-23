package federation

import (
	"container/list"
	"sync"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/proxy"
)

type clientEntry struct {
	id       string
	client   proxy.AWSClient
	target   Target
	lastUsed time.Time
	element  *list.Element
}

type clientPool struct {
	mu      sync.Mutex
	max     int
	idleTTL time.Duration
	now     func() time.Time
	entries map[string]*clientEntry
	lru     *list.List
}

func newClientPool(max int, idleTTL time.Duration) *clientPool {
	if max <= 0 {
		max = 10000
	}
	if idleTTL <= 0 {
		idleTTL = 15 * time.Minute
	}
	return &clientPool{
		max:     max,
		idleTTL: idleTTL,
		now:     time.Now,
		entries: make(map[string]*clientEntry),
		lru:     list.New(),
	}
}

func (p *clientPool) get(target Target) (proxy.AWSClient, bool) {
	if p == nil {
		return nil, false
	}
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	entry := p.entries[target.ID]
	if entry == nil {
		return nil, false
	}
	if !sameClientTarget(entry.target, target) || now.Sub(entry.lastUsed) > p.idleTTL {
		p.removeLocked(entry)
		return nil, false
	}
	entry.lastUsed = now
	p.lru.MoveToFront(entry.element)
	return entry.client, true
}

func (p *clientPool) put(target Target, client proxy.AWSClient) proxy.AWSClient {
	if p == nil {
		return client
	}
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if existing := p.entries[target.ID]; existing != nil {
		existing.lastUsed = now
		p.lru.MoveToFront(existing.element)
		return existing.client
	}
	entry := &clientEntry{id: target.ID, client: client, target: target, lastUsed: now}
	entry.element = p.lru.PushFront(entry)
	p.entries[target.ID] = entry
	p.evictLocked(now)
	return client
}

func (p *clientPool) prune(valid func(Target) bool) {
	if p == nil {
		return
	}
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, entry := range p.entries {
		if !valid(entry.target) || now.Sub(entry.lastUsed) > p.idleTTL {
			p.removeLocked(entry)
		}
	}
}

func (p *clientPool) evictLocked(now time.Time) {
	for element := p.lru.Back(); element != nil; {
		entry, _ := element.Value.(*clientEntry)
		previous := element.Prev()
		if len(p.entries) > p.max || (entry != nil && now.Sub(entry.lastUsed) > p.idleTTL) {
			if entry != nil {
				p.removeLocked(entry)
			}
		}
		element = previous
	}
}

func (p *clientPool) removeLocked(entry *clientEntry) {
	if entry == nil {
		return
	}
	delete(p.entries, entry.id)
	if entry.element != nil {
		p.lru.Remove(entry.element)
	}
}

func (p *clientPool) len() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}
