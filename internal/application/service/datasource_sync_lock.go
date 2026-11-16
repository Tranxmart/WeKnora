package service

import (
	"context"
	"sync"
)

// dataSourceSyncLocks serializes cursor readers and writers for each source in
// this backend. Deployments using this connector must run a single backend replica.
// Entries live only while an execution owns or waits for the lock.
type dataSourceSyncLocks struct {
	mu      sync.Mutex
	entries map[string]*dataSourceSyncLock
}

type dataSourceSyncLock struct {
	token chan struct{}
	refs  int
}

func (l *dataSourceSyncLocks) acquire(ctx context.Context, id string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.mu.Lock()
	if l.entries == nil {
		l.entries = make(map[string]*dataSourceSyncLock)
	}
	entry := l.entries[id]
	if entry == nil {
		entry = &dataSourceSyncLock{token: make(chan struct{}, 1)}
		entry.token <- struct{}{}
		l.entries[id] = entry
	}
	entry.refs++
	l.mu.Unlock()

	dropRef := func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		entry.refs--
		if entry.refs == 0 {
			delete(l.entries, id)
		}
	}
	select {
	case <-ctx.Done():
		dropRef()
		return nil, ctx.Err()
	case <-entry.token:
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			entry.token <- struct{}{}
			dropRef()
		})
	}
	if err := ctx.Err(); err != nil {
		release()
		return nil, err
	}
	return release, nil
}
