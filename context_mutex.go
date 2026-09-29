package thalovant

import (
	"context"
	"sync"
)

// Zero-value mutex whose queued callers can cancel without acquiring ownership.
type contextMutex struct {
	once  sync.Once
	token chan struct{}
}

func (m *contextMutex) Lock(ctx context.Context) error {
	m.once.Do(func() { m.token = make(chan struct{}, 1) })
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case m.token <- struct{}{}:
		if err := ctx.Err(); err != nil {
			m.Unlock()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (m *contextMutex) Unlock() { <-m.token }

// noiseLock is a noiseChannel's lock: a plain mutex for its short sections,
// whose wait a send can also withdraw (LockContext) while it is still queued
// behind another frame.
type noiseLock struct{ inner contextMutex }

func (m *noiseLock) Lock()                                 { _ = m.inner.Lock(context.Background()) }
func (m *noiseLock) Unlock()                               { m.inner.Unlock() }
func (m *noiseLock) LockContext(ctx context.Context) error { return m.inner.Lock(ctx) }
