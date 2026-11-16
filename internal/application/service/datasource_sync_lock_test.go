package service

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/require"
)

func TestDataSourceSyncLocksSerializeAndRelease(t *testing.T) {
	var locks dataSourceSyncLocks
	var active, visits atomic.Int32
	var wg sync.WaitGroup
	for range 30 {
		wg.Go(func() {
			release, err := locks.acquire(context.Background(), "same-source")
			require.NoError(t, err)
			n := active.Add(1)
			if n != 1 {
				t.Errorf("overlapping executions: %d", n)
			}
			visits.Add(1)
			active.Add(-1)
			release()
		})
	}
	wg.Wait()
	require.EqualValues(t, 30, visits.Load())
	require.Empty(t, locks.entries)
}

func TestDataSourceSyncLockCancellationAndIndependentKeys(t *testing.T) {
	var locks dataSourceSyncLocks
	release, err := locks.acquire(context.Background(), "one")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = locks.acquire(ctx, "one")
	require.ErrorIs(t, err, context.Canceled)
	releaseOther, err := locks.acquire(context.Background(), "two")
	require.NoError(t, err)
	releaseOther()
	release()
	require.Empty(t, locks.entries)
}

func TestProcessSyncWaitsBeforeReadingCursor(t *testing.T) {
	svc := &DataSourceService{} // Any repository access would panic.
	release, err := svc.syncLocks.acquire(context.Background(), "source")
	require.NoError(t, err)
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- svc.ProcessSync(ctx, asynq.NewTask(types.TypeDataSourceSync, []byte(`{"data_source_id":"source"}`)))
	}()
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}
