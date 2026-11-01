package ruler

import (
	"container/heap"
	"context"
	"hash/fnv"
	"log"
	"sync"
	"time"
)

// GroupEvaluator evaluates one group and returns per-rule results.
type GroupEvaluator interface {
	EvaluateGroup(ctx context.Context, group Group) []EvalResult
}

// Scheduler reloads rule groups and evaluates them on their intervals using a
// bounded worker pool.
type Scheduler struct {
	Evaluator    GroupEvaluator
	Load         func(ctx context.Context) ([]GroupRef, error)
	Workers      int
	PollInterval time.Duration
	Logger       *log.Logger
	Now          func() time.Time

	mu    sync.Mutex
	stats SchedulerStats
}

// SchedulerStats reports scheduler state for health endpoints.
type SchedulerStats struct {
	Groups    int       `json:"groups"`
	Due       int64     `json:"due"`
	Evaluated int64     `json:"evaluated"`
	LastLoad  time.Time `json:"lastLoad"`
	LastError string    `json:"lastError,omitempty"`
}

// Stats returns a snapshot of scheduler counters.
func (s *Scheduler) Stats() SchedulerStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

type scheduledGroup struct {
	ref      GroupRef
	interval time.Duration
	next     time.Time
	index    int
}

type groupHeap []*scheduledGroup

func (h groupHeap) Len() int           { return len(h) }
func (h groupHeap) Less(i, j int) bool { return h[i].next.Before(h[j].next) }
func (h groupHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i]; h[i].index = i; h[j].index = j }
func (h *groupHeap) Push(x any) {
	item := x.(*scheduledGroup)
	item.index = len(*h)
	*h = append(*h, item)
}
func (h *groupHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	old[n-1] = nil
	item.index = -1
	*h = old[:n-1]
	return item
}

// Run evaluates groups until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	if s.Workers <= 0 {
		s.Workers = 8
	}
	if s.PollInterval <= 0 {
		s.PollInterval = 30 * time.Second
	}
	if s.Now == nil {
		s.Now = func() time.Time { return time.Now().UTC() }
	}

	groups := make(map[string]*scheduledGroup)
	pending := &groupHeap{}
	heap.Init(pending)

	reload := func() {
		refs, err := s.Load(ctx)
		if err != nil {
			s.setLoadError(err)
			return
		}
		seen := make(map[string]struct{}, len(refs))
		now := s.Now()
		for _, ref := range refs {
			key := ref.Tenant + "/" + ref.Group
			seen[key] = struct{}{}
			if existing, ok := groups[key]; ok {
				existing.ref = ref
				continue
			}
			interval := groupInterval(ref)
			group := &scheduledGroup{
				ref:      ref,
				interval: interval,
				next:     now.Add(jitter(key, interval)),
			}
			groups[key] = group
			heap.Push(pending, group)
		}
		for key, group := range groups {
			if _, ok := seen[key]; !ok {
				if group.index >= 0 {
					heap.Remove(pending, group.index)
				}
				delete(groups, key)
			}
		}
		s.mu.Lock()
		s.stats.Groups = len(groups)
		s.stats.LastLoad = now
		s.stats.LastError = ""
		s.mu.Unlock()
	}

	reload()
	reloadTicker := time.NewTicker(s.PollInterval)
	defer reloadTicker.Stop()

	sem := make(chan struct{}, s.Workers)
	var wg sync.WaitGroup

	for {
		now := s.Now()
		if pending.Len() == 0 || !(*pending)[0].next.After(now) {
			if pending.Len() == 0 {
				select {
				case <-ctx.Done():
					wg.Wait()
					return
				case <-reloadTicker.C:
					reload()
					continue
				}
			}
			group := heap.Pop(pending).(*scheduledGroup)
			// Skip missed intervals instead of walking every interval that
			// elapsed while the process was paused. Recording rules should run
			// at the next future boundary, not replay hours of catch-up work.
			if !group.next.After(now) {
				group.next = now.Add(group.interval)
			}
			heap.Push(pending, group)

			s.mu.Lock()
			s.stats.Due++
			s.stats.Evaluated++
			s.mu.Unlock()

			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				wg.Wait()
				return
			}
			wg.Add(1)
			go func(ref GroupRef) {
				defer wg.Done()
				defer func() { <-sem }()
				results := s.Evaluator.EvaluateGroup(ctx, groupForRef(ref))
				for _, result := range results {
					if result.Error != "" && s.Logger != nil {
						s.Logger.Printf("rules: group=%s rule=%s error=%s", result.Group, result.Rule, result.Error)
					}
				}
			}(group.ref)
			continue
		}

		wait := time.Until((*pending)[0].next)
		if s.Now != nil {
			wait = (*pending)[0].next.Sub(s.Now())
		}
		if wait > time.Second {
			wait = time.Second
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			wg.Wait()
			return
		case <-reloadTicker.C:
			timer.Stop()
			reload()
		case <-timer.C:
		}
	}
}

func (s *Scheduler) setLoadError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.LastError = err.Error()
}

func groupInterval(ref GroupRef) time.Duration {
	if ref.File == nil || len(ref.File.Groups) == 0 {
		return time.Minute
	}
	return ref.File.Groups[0].IntervalOrDefault()
}

func groupForRef(ref GroupRef) Group {
	if ref.File == nil || len(ref.File.Groups) == 0 {
		return Group{Tenant: ref.Tenant, Name: ref.Group}
	}
	group := ref.File.Groups[0]
	if group.Tenant == "" {
		group.Tenant = ref.Tenant
	}
	return group
}

// jitter spreads group evaluations within the first 10% of the interval.
func jitter(key string, interval time.Duration) time.Duration {
	if interval <= 0 {
		return 0
	}
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(key))
	spread := interval / 10
	if spread <= 0 {
		return 0
	}
	return time.Duration(hasher.Sum32()%uint32(spread/time.Millisecond)) * time.Millisecond
}
