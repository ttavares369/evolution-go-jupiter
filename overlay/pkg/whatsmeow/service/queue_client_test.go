package whatsmeow_service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
)

const queueLeaseTestID = "00000000-0000-4000-8000-000000000001"

func queueLeaseRuntime() (*whatsmeowService, *MyClient) {
	client := whatsmeow.NewClient(&store.Device{}, nil)
	mycli := &MyClient{WAClient: client, done: make(chan struct{}), stopChannel: make(chan bool, 1)}
	w := &whatsmeowService{
		lifecycle:       &runtimeLifecycle{starting: make(map[string]bool), restartLocks: make(map[string]*sync.Mutex)},
		clientPointer:   make(map[string]*whatsmeow.Client),
		myClientPointer: make(map[string]*MyClient),
		killChannel:     make(map[string]chan bool),
	}
	w.setRuntimePointers(queueLeaseTestID, client, mycli, mycli.stopChannel)
	return w, mycli
}

func queueLeaseAwait(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for lifecycle synchronization")
	}
}

func TestAcquireQueueClientDisconnectedRuntimeNeedsNoNetwork(t *testing.T) {
	w, mycli := queueLeaseRuntime()
	client, ctx, release, err := AcquireQueueClient(w, context.Background(), queueLeaseTestID)
	if err != nil {
		t.Fatal(err)
	}
	if client != mycli.WAClient || client.IsConnected() || client.IsLoggedIn() || ctx.Err() != nil {
		t.Fatal("acquire must borrow, not connect or validate readiness")
	}
	release()
	release()
	queueLeaseAwait(t, ctx.Done())
	mycli.markStopping()
	stopped := make(chan struct{})
	go func() { mycli.shutdownWorkers(); close(stopped) }()
	queueLeaseAwait(t, stopped)
}

func TestAcquireQueueClientShutdownCancelsButWaitsForRelease(t *testing.T) {
	w, mycli := queueLeaseRuntime()
	_, ctx, release, err := AcquireQueueClient(w, context.Background(), queueLeaseTestID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	mycli.markStopping()
	stopped := make(chan struct{})
	go func() { mycli.shutdownWorkers(); close(stopped) }()
	queueLeaseAwait(t, ctx.Done())
	select {
	case <-stopped:
		t.Fatal("shutdown passed a live lease")
	default:
	}
	if _, _, releaseFailed, err := AcquireQueueClient(w, context.Background(), queueLeaseTestID); err == nil {
		releaseFailed()
		t.Fatal("acquired stopping runtime")
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() { defer wg.Done(); release() }()
	}
	wg.Wait()
	queueLeaseAwait(t, stopped)
}

func TestAcquireQueueClientParentCancellationDoesNotReleaseActiveUse(t *testing.T) {
	w, mycli := queueLeaseRuntime()
	parent, cancel := context.WithCancel(context.Background())
	_, ctx, release, err := AcquireQueueClient(w, parent, queueLeaseTestID)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	defer release()
	cancel()
	queueLeaseAwait(t, ctx.Done())
	mycli.markStopping()
	stopped := make(chan struct{})
	go func() { mycli.shutdownWorkers(); close(stopped) }()
	queueLeaseAwait(t, mycli.done)
	select {
	case <-stopped:
		t.Fatal("cancel released an active lease")
	default:
	}
	release()
	queueLeaseAwait(t, stopped)
}

type queueUnsupportedService struct{ WhatsmeowService }

func TestAcquireQueueClientRefusesMissingOrInvalidRuntimes(t *testing.T) {
	for _, mode := range []string{"nil", "typed-nil", "unsupported", "uninitialized", "absent", "mismatched", "missing-done", "stopping", "stopped", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			w, mycli := queueLeaseRuntime()
			var svc WhatsmeowService = w
			ctx := context.Background()
			switch mode {
			case "nil":
				svc = nil
			case "typed-nil":
				svc = (*whatsmeowService)(nil)
			case "unsupported":
				svc = &queueUnsupportedService{}
			case "uninitialized":
				svc = &whatsmeowService{}
			case "absent":
				w.clearRuntimePointers(queueLeaseTestID, mycli.WAClient, mycli, mycli.stopChannel)
			case "mismatched":
				mycli.WAClient = whatsmeow.NewClient(&store.Device{}, nil)
			case "missing-done":
				mycli.done = nil
			case "stopping":
				mycli.markStopping()
			case "stopped":
				mycli.closeDone()
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			client, leased, release, err := AcquireQueueClient(svc, ctx, queueLeaseTestID)
			if err == nil || client != nil || leased != nil {
				t.Fatalf("client=%v ctx=%v err=%v", client, leased, err)
			}
			if mode == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			release()
			release()
			mycli.markStopping()
			// No cleanup path may leak the worker registered before detecting done.
			waited := make(chan struct{})
			go func() { mycli.workerWG.Wait(); close(waited) }()
			queueLeaseAwait(t, waited)
		})
	}
}

func TestAcquireQueueClientStartStopRace(t *testing.T) {
	for range 50 {
		w, mycli := queueLeaseRuntime()
		start := make(chan struct{})
		var clients sync.WaitGroup
		for range 12 {
			clients.Add(1)
			go func() {
				defer clients.Done()
				<-start
				client, ctx, release, err := AcquireQueueClient(w, context.Background(), queueLeaseTestID)
				if err != nil {
					release()
					return
				}
				defer release()
				if client != mycli.WAClient {
					t.Error("wrong runtime")
				}
				select {
				case <-ctx.Done():
				case <-time.After(3 * time.Second):
					t.Error("lease not canceled on stop")
				}
			}()
		}
		stopped := make(chan struct{})
		go func() {
			<-start
			mycli.markStopping()
			mycli.shutdownWorkers()
			w.clearRuntimePointers(queueLeaseTestID, mycli.WAClient, mycli, mycli.stopChannel)
			close(stopped)
		}()
		close(start)
		clients.Wait()
		queueLeaseAwait(t, stopped)
	}
}

func TestAcquireQueueClientConcurrentPublishAndLookup(t *testing.T) {
	w, mycli := queueLeaseRuntime()
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		for range 100 {
			w.clearRuntimePointers(queueLeaseTestID, mycli.WAClient, mycli, mycli.stopChannel)
			w.setRuntimePointers(queueLeaseTestID, mycli.WAClient, mycli, mycli.stopChannel)
		}
	}()
	go func() {
		defer workers.Done()
		for range 100 {
			client, _, release, err := AcquireQueueClient(w, context.Background(), queueLeaseTestID)
			if err == nil && client != mycli.WAClient {
				t.Error("inconsistent snapshot")
			}
			release()
		}
	}()
	workers.Wait()
	mycli.markStopping()
	stopped := make(chan struct{})
	go func() { mycli.shutdownWorkers(); close(stopped) }()
	queueLeaseAwait(t, stopped)
}
