package whatsmeow_service

import (
	"context"
	"errors"
	"sync"

	"go.mau.fi/whatsmeow"
)

// AcquireQueueClient borrows the current runtime without starting or reconnecting
// it. The caller must release only after all uses of the client have returned.
// Cancellation does not prove that an underlying websocket write has stopped.
func AcquireQueueClient(svc WhatsmeowService, ctx context.Context, instanceID string) (*whatsmeow.Client, context.Context, func(), error) {
	noop := func() {}
	if err := ctx.Err(); err != nil {
		return nil, nil, noop, err
	}
	w, ok := svc.(*whatsmeowService)
	if !ok || w == nil || w.lifecycle == nil || instanceID == "" {
		return nil, nil, noop, errors.New("queue client runtime unavailable")
	}
	client, mycli, _ := w.runtimePointers(instanceID)
	if client == nil || mycli == nil || mycli.WAClient != client || mycli.done == nil || !mycli.startWorker() {
		return nil, nil, noop, errors.New("queue client runtime unavailable or stopping")
	}

	leasedCtx, cancel := context.WithCancel(ctx)
	var once sync.Once
	release := func() {
		once.Do(func() {
			cancel()
			mycli.workerWG.Done()
		})
	}
	select {
	case <-mycli.done:
		release()
		return nil, nil, noop, errors.New("queue client runtime stopped")
	default:
	}
	if err := leasedCtx.Err(); err != nil {
		release()
		return nil, nil, noop, err
	}
	go func() {
		select {
		case <-mycli.done:
			cancel()
		case <-leasedCtx.Done():
		}
	}()
	return client, leasedCtx, release, nil
}
