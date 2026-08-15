package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

// awaitCallback returns the error Supervise reported, or (nil,false) if nothing
// fired within the timeout. Supervise runs its watch in a goroutine, so tests
// synchronize through this channel rather than sleeping.
func awaitCallback(t *testing.T, fired <-chan error, timeout time.Duration) (error, bool) {
	t.Helper()
	select {
	case err := <-fired:
		return err, true
	case <-time.After(timeout):
		return nil, false
	}
}

func TestSuperviseReportsCrash(t *testing.T) {
	exit := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := &nixPostgres{serverExit: exit, serverCtx: ctx}

	fired := make(chan error, 1)
	n.Supervise(func(err error) { fired <- err })

	// The postmaster dies mid-run without anyone calling Stop.
	crash := errors.New("postgres: terminated by signal 9")
	exit <- crash

	got, ok := awaitCallback(t, fired, 2*time.Second)
	if !ok {
		t.Fatal("Supervise did not report an unexpected exit")
	}
	if !errors.Is(got, crash) {
		t.Fatalf("reported error = %v, want %v", got, crash)
	}
}

func TestSuperviseReportsCleanExit(t *testing.T) {
	exit := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := &nixPostgres{serverExit: exit, serverCtx: ctx}

	fired := make(chan error, 1)
	n.Supervise(func(err error) { fired <- err })

	// A clean exit mid-run (nil error) is still unexpected: nobody asked
	// postgres to stop, yet it is gone. It must be reported.
	exit <- nil

	got, ok := awaitCallback(t, fired, 2*time.Second)
	if !ok {
		t.Fatal("Supervise did not report a clean mid-run exit")
	}
	if got != nil {
		t.Fatalf("reported error = %v, want nil", got)
	}
}

func TestSuperviseIgnoresDeliberateStop(t *testing.T) {
	exit := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	n := &nixPostgres{serverExit: exit, serverCtx: ctx}

	fired := make(chan error, 1)
	n.Supervise(func(err error) { fired <- err })

	// Stop cancels serverCtx BEFORE the process terminates, then the process
	// exit lands on the channel. The cancelled context must suppress the report.
	cancel()
	exit <- errors.New("postgres: shutting down")

	if _, ok := awaitCallback(t, fired, 500*time.Millisecond); ok {
		t.Fatal("Supervise reported a deliberate Stop as an unexpected exit")
	}
}

func TestSuperviseNilChannelIsNoop(t *testing.T) {
	// Before startServer (e.g. Docker runtime) serverExit is nil. Supervise must
	// not spawn a goroutine or panic.
	n := &nixPostgres{}
	n.Supervise(func(error) { t.Fatal("callback fired with no server to watch") })
	time.Sleep(50 * time.Millisecond)
}
