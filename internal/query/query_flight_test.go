package query

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestQueryFlightKeepsWorkAliveForRemainingWaiter(t *testing.T) {
	var group queryFlightGroup
	started := make(chan struct{})
	canceled := make(chan struct{})
	fn := func(ctx context.Context) (proxyQueryResult, error) {
		close(started)
		<-ctx.Done()
		close(canceled)
		return proxyQueryResult{}, ctx.Err()
	}

	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())
	done1 := make(chan error, 1)
	done2 := make(chan error, 1)
	go func() {
		_, err := group.Do(ctx1, "k", fn)
		done1 <- err
	}()
	<-started
	go func() {
		_, err := group.Do(ctx2, "k", fn)
		done2 <- err
	}()
	// Give the second waiter time to register.
	time.Sleep(20 * time.Millisecond)

	cancel1()
	select {
	case err := <-done1:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("first waiter: expected canceled, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("first waiter did not return")
	}
	select {
	case <-canceled:
		t.Fatal("shared work was canceled while a waiter was still active")
	default:
	}

	cancel2()
	select {
	case err := <-done2:
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("second waiter: expected cancellation, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("second waiter did not return")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("shared work was not canceled after the last waiter left")
	}
}
