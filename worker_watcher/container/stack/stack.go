package stack

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/roadrunner-server/api/v2/worker"
	"github.com/roadrunner-server/errors"
)

type Stack struct {
	mu sync.Mutex

	// container size
	len uint64
	// destroy signal
	destroy uint64
	// reset signal
	reset uint64

	nextIndex int64
	workers   []worker.BaseProcess
}

func NewStack(len uint64) *Stack {
	return &Stack{
		mu:        sync.Mutex{},
		len:       len,
		destroy:   0,
		reset:     0,
		nextIndex: 0,
		workers:   make([]worker.BaseProcess, len),
	}
}

func (s *Stack) Push(w worker.BaseProcess) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.workers[s.nextIndex] = w
	atomic.AddInt64(&s.nextIndex, 1)
}

func (s *Stack) Len() int {
	return int(s.nextIndex)
}

func (s *Stack) Remove(_ int64) {}

func (s *Stack) Pop(ctx context.Context) (worker.BaseProcess, error) {
	// remove all workers and return
	if atomic.LoadUint64(&s.destroy) == 1 {
		// drain
		s.Drain()
		return nil, errors.E(errors.WatcherStopped)
	}

	// wait for the reset to complete
	for atomic.CompareAndSwapUint64(&s.reset, 1, 1) {
		time.Sleep(time.Millisecond)
	}

	// used only for the TTL-ed workers
	s.mu.Lock()
	defer s.mu.Unlock()

	select {
	case <-ctx.Done():
		return nil, errors.E(ctx.Err(), errors.NoFreeWorkers)
	default:
		index := atomic.AddInt64(&s.nextIndex, -1)
		if index < 0 {
			atomic.StoreInt64(&s.nextIndex, 0)
			return nil, errors.E("No free workers", errors.NoFreeWorkers)
		}
		w := s.workers[index]
		s.workers[index] = nil
		return w, nil
	}
}

func (s *Stack) Drain() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.workers = make([]worker.BaseProcess, s.len)
	atomic.StoreInt64(&s.nextIndex, 0)
}

func (s *Stack) ResetDone() {
	atomic.StoreUint64(&s.reset, 0)
}

func (s *Stack) Reset() {
	atomic.StoreUint64(&s.reset, 1)
}

func (s *Stack) Destroy() {
	atomic.StoreUint64(&s.destroy, 1)
}
