package worker_watcher //nolint:stylecheck

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/roadrunner-server/api/v2/event_bus"
	"github.com/roadrunner-server/api/v2/worker"
	"github.com/roadrunner-server/errors"
	"github.com/roadrunner-server/sdk/v2/events"
	"github.com/roadrunner-server/sdk/v2/utils"
	"github.com/roadrunner-server/sdk/v2/worker_watcher/container/channel"
	"go.uber.org/zap"
)

type workerWatcher struct {
	sync.RWMutex
	// actually don't have a lot of impl here, so interface not needed
	container *channel.Vec
	// used to control Destroy stage (that all workers are in the container)
	numWorkers *uint64
	eventBus   event_bus.EventBus

	workers []worker.BaseProcess
	log     *zap.Logger

	allocator       worker.Allocator
	allocateTimeout time.Duration
}

// NewSyncWorkerWatcher is a constructor for the Watcher
func NewSyncWorkerWatcher(allocator worker.Allocator, log *zap.Logger, numWorkers uint64, allocateTimeout time.Duration) *workerWatcher {
	eb, _ := events.Bus()
	return &workerWatcher{
		container: channel.NewVector(numWorkers),

		log:      log,
		eventBus: eb,
		// pass a ptr to the number of workers to avoid blocking in the TTL loop
		numWorkers:      utils.Uint64(numWorkers),
		allocateTimeout: allocateTimeout,
		workers:         make([]worker.BaseProcess, 0, numWorkers),

		allocator: allocator,
	}
}

func (ww *workerWatcher) Watch(workers []worker.BaseProcess) error {
	ww.Lock()
	defer ww.Unlock()
	for i := 0; i < len(workers); i++ {
		ii := i
		ww.container.Push(workers[ii])
		// add worker to watch slice
		ww.workers = append(ww.workers, workers[ii])
		ww.addToWatch(workers[ii])
	}
	return nil
}

// Take is not a thread safe operation
func (ww *workerWatcher) Take(ctx context.Context) (worker.BaseProcess, error) {
	const op = errors.Op("worker_watcher_get_free_worker")
	// we need lock here to prevent Pop operation when ww in the resetting state
	// thread safe operation
	w, err := ww.container.Pop(ctx)

	if err != nil {
		if errors.Is(errors.WatcherStopped, err) {
			return nil, errors.E(op, errors.WatcherStopped)
		}

		return nil, errors.E(op, err)
	}

	// fast path, worker not nil and in the ReadyState
	if w.State().Value() == worker.StateReady {
		return w, nil
	}

	// =========================================================
	// SLOW PATH
	_ = w.Kill()
	// no free workers in the container or worker not in the ReadyState (TTL-ed)
	// try to continuously get free one
	for {
		w, err = ww.container.Pop(ctx)
		if err != nil {
			if errors.Is(errors.WatcherStopped, err) {
				return nil, errors.E(op, errors.WatcherStopped)
			}
			return nil, errors.E(op, err)
		}

		switch w.State().Value() {
		// return only workers in the Ready state
		// check first
		case worker.StateReady:
			return w, nil
		case worker.StateWorking: // how??
			ww.container.Push(w) // put it back, let worker finish the work
			continue
		case
			// all the possible wrong states
			worker.StateInactive,
			worker.StateDestroyed,
			worker.StateErrored,
			worker.StateStopped,
			worker.StateInvalid,
			worker.StateKilling,
			worker.StateStopping:
			// worker doing no work because it in the container
			// so we can safely kill it (inconsistent state)
			_ = w.Kill()
			// try to get new worker
			continue
		}
	}
}

func (ww *workerWatcher) Allocate() error {
	const op = errors.Op("worker_watcher_allocate_new")

	sw, err := ww.allocator()
	if err != nil {
		// log incident
		ww.log.Error("allocate", zap.Error(err))
		// if no timeout, return error immediately
		if ww.allocateTimeout == 0 {
			return errors.E(op, errors.WorkerAllocate, err)
		}

		// every second
		allocateFreq := time.NewTicker(time.Millisecond * 1000)

		tt := time.After(ww.allocateTimeout)
		for {
			select {
			case <-tt:
				// reduce number of workers
				atomic.AddUint64(ww.numWorkers, ^uint64(0))
				allocateFreq.Stop()
				// timeout exceed, worker can't be allocated
				return errors.E(op, errors.WorkerAllocate, err)

			case <-allocateFreq.C:
				sw, err = ww.allocator()
				if err != nil {
					// log incident
					ww.log.Error("allocate retry attempt failed", zap.String("internal_event_name", events.EventWorkerError.String()), zap.Error(err))
					continue
				}

				// reallocated
				allocateFreq.Stop()
				goto done
			}
		}
	}

done:
	// add worker to Wait
	ww.addToWatch(sw)

	ww.Lock()
	// add new worker to the workers slice (to get information about workers in parallel)
	ww.workers = append(ww.workers, sw)
	ww.Unlock()

	// push the worker to the container
	ww.Release(sw)
	return nil
}

// Remove worker
func (ww *workerWatcher) Remove(wb worker.BaseProcess) {
	ww.Lock()
	defer ww.Unlock()

	// set remove state
	pid := wb.Pid()

	// worker will be removed on the Get operation
	for i := 0; i < len(ww.workers); i++ {
		if ww.workers[i].Pid() == pid {
			ww.workers = append(ww.workers[:i], ww.workers[i+1:]...)
			return
		}
	}
}

// Release O(1) operation
func (ww *workerWatcher) Release(w worker.BaseProcess) {
	switch w.State().Value() {
	case worker.StateReady:
		ww.container.Push(w)
	default:
		_ = w.Kill()
	}
}

func (ww *workerWatcher) Reset(ctx context.Context) {
	ww.Lock()
	// do not release new workers
	ww.container.Reset()
	ww.Unlock()

	tt := time.NewTicker(time.Millisecond * 10)
	defer tt.Stop()
	for {
		select {
		case <-tt.C:
			ww.RLock()

			if ww.container.Len() == 0 {
				ww.RUnlock()
				goto drain
			}

			// that might be one of the workers is working
			// to proceed, all workers should be inside a channel
			if atomic.LoadUint64(ww.numWorkers) != uint64(ww.container.Len()) {
				ww.RUnlock()
				continue
			}
			ww.RUnlock()
			// All workers at this moment are in the container
			// Pop operation is blocked, push can't be done, since it's not possible to pop
		drain:
			ww.Lock()

			// drain channel
			ww.container.Drain()
			for i := 0; i < len(ww.workers); i++ {
				ww.workers[i].State().Set(worker.StateDestroyed)
				// kill the worker
				_ = ww.workers[i].Kill()
			}

			ww.workers = make([]worker.BaseProcess, 0, atomic.LoadUint64(ww.numWorkers))
			ww.container.ResetDone()
			ww.Unlock()
			return
		case <-ctx.Done():
			// drain channel
			ww.container.Drain()
			// kill workers
			ww.Lock()
			// drain workers slice
			for i := 0; i < len(ww.workers); i++ {
				ww.workers[i].State().Set(worker.StateDestroyed)
				// kill the worker
				_ = ww.workers[i].Kill()
			}

			ww.workers = make([]worker.BaseProcess, 0, atomic.LoadUint64(ww.numWorkers))
			ww.container.ResetDone()
			ww.Unlock()
			return
		}
	}
}

// Destroy all underlying container (but let them complete the task)
func (ww *workerWatcher) Destroy(ctx context.Context) {
	ww.Lock()
	// do not release new workers
	ww.container.Destroy()
	ww.Unlock()

	tt := time.NewTicker(time.Millisecond * 10)
	// destroy container, we don't use ww mutex here, since we should be able to push worker
	defer tt.Stop()
	for {
		select {
		case <-tt.C:
			/*
				Edge case mostly occurred in reload plugin. When worker is broken, RR can't allocate more of them.
				So, there is 0 ww.workers len, and it is never equal to the ww.numWorkers
			*/
			ww.RLock()
			if ww.container.Len() == 0 {
				ww.RUnlock()
				goto drain
			}

			// that might be one of the workers is working
			if atomic.LoadUint64(ww.numWorkers) != uint64(ww.container.Len()) {
				ww.RUnlock()
				continue
			}
			ww.RUnlock()
			// All container at this moment are in the container
			// Pop operation is blocked, push can't be done, since it's not possible to pop

		drain:
			ww.Lock()
			// drain channel
			_, _ = ww.container.Pop(ctx)
			for i := 0; i < len(ww.workers); i++ {
				ww.workers[i].State().Set(worker.StateDestroyed)
				// kill the worker
				_ = ww.workers[i].Kill()
			}
			ww.Unlock()
			return
		case <-ctx.Done():
			// drain channel
			_, _ = ww.container.Pop(ctx)
			// kill workers
			ww.Lock()
			for i := 0; i < len(ww.workers); i++ {
				ww.workers[i].State().Set(worker.StateDestroyed)
				// kill the worker
				_ = ww.workers[i].Kill()
			}
			ww.Unlock()
			return
		}
	}
}

// List - this is O(n) operation, and it will return copy of the actual workers
func (ww *workerWatcher) List() []worker.BaseProcess {
	ww.RLock()
	defer ww.RUnlock()

	if len(ww.workers) == 0 {
		return nil
	}

	base := make([]worker.BaseProcess, 0, len(ww.workers))
	for i := 0; i < len(ww.workers); i++ {
		base = append(base, ww.workers[i])
	}

	return base
}

func (ww *workerWatcher) wait(w worker.BaseProcess) {
	const op = errors.Op("worker_watcher_wait")
	err := w.Wait()
	if err != nil {
		ww.log.Debug("worker stopped", zap.String("internal_event_name", events.EventWorkerWaitExit.String()), zap.Error(err))
	}

	// remove worker
	ww.Remove(w)

	if w.State().Value() == worker.StateDestroyed {
		// worker was manually destroyed, no need to replace
		ww.log.Debug("worker destroyed", zap.Int64("pid", w.Pid()), zap.String("internal_event_name", events.EventWorkerDestruct.String()), zap.Error(err))
		return
	}

	err = ww.Allocate()
	if err != nil {
		ww.log.Error("failed to allocate the worker", zap.String("internal_event_name", events.EventWorkerError.String()), zap.Error(err))

		// no workers at all, panic
		ww.Lock()
		defer ww.Unlock()

		if len(ww.workers) == 0 && atomic.LoadUint64(ww.numWorkers) == 0 {
			panic(errors.E(op, errors.WorkerAllocate, errors.Errorf("can't allocate workers: %v, no workers in the pool", err)))
		}
	}

	// this event used mostly for the temporal plugin
	ww.eventBus.Send(events.NewEvent(events.EventWorkerStopped, "worker_watcher", fmt.Sprintf("process exited, pid: %d", w.Pid())))
}

func (ww *workerWatcher) addToWatch(wb worker.BaseProcess) {
	go func() {
		ww.wait(wb)
	}()
}
