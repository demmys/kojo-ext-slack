package slackbot

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestGoalHandoffWaitsForSlackSourceFIFORelease(t *testing.T) {
	ready := make(chan struct{})
	r := &slackHandoffReservation{reservation: &threadReservation{ready: ready}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := r.WaitSourceComplete(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("source still finalizing: %v", err)
	}
	close(ready)
	if err := r.WaitSourceComplete(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.sourceCompleteErr = errors.New("history write failed")
	if err := r.WaitSourceComplete(context.Background()); err == nil {
		t.Fatal("history persistence failure ignored")
	}
}
