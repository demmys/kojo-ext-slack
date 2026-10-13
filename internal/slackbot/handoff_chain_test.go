package slackbot

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/loppo-llc/kojo-ext-slack/internal/agent"
)

func waitChainSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("FIFO did not advance")
	}
}

func assertChainBlocked(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
		t.Fatal("later turn overtook handoff chain")
	default:
	}
}

func TestThreadSuccessorRetainsPublishedFIFOAndTail(t *testing.T) {
	b := newTestBot(t, agent.SlackBotConfig{})
	defer b.cancel()
	source, arrival := b.reserveThreadPair("C", "T")
	later := b.reserveThread("C", "T")
	b.releaseThreadReservation("C", "T", source)
	arrival.Wait()
	next, err := b.reserveThreadSuccessor("C", "T", arrival)
	if err != nil {
		t.Fatal(err)
	}
	b.releaseThreadReservation("C", "T", arrival)
	next.Wait()
	assertChainBlocked(t, later.ready)
	last, err := b.reserveThreadSuccessor("C", "T", next)
	if err != nil {
		t.Fatal(err)
	}
	b.releaseThreadReservation("C", "T", next)
	last.Wait()
	assertChainBlocked(t, later.ready)
	b.releaseThreadReservation("C", "T", last)
	later.Wait()
	// Tail case: a new ordinary ticket must wait on successor, not current.
	tail, err := b.reserveThreadSuccessor("C", "T", later)
	if err != nil {
		t.Fatal(err)
	}
	newer := b.reserveThread("C", "T")
	b.releaseThreadReservation("C", "T", later)
	tail.Wait()
	assertChainBlocked(t, newer.ready)
	b.releaseThreadReservation("C", "T", tail)
	newer.Wait()
	b.releaseThreadReservation("C", "T", newer)
	b.releaseThreadReservation("C", "T", newer) // does not drop refcount twice
	if _, err := b.reserveThreadSuccessor("C", "T", newer); err == nil {
		t.Fatal("split released ticket")
	}
	if len(b.threadLocks) != 0 {
		t.Fatal("FIFO references leaked")
	}
}

func TestThreadSuccessorConcurrentRelease(t *testing.T) {
	b := newTestBot(t, agent.SlackBotConfig{})
	defer b.cancel()
	for range 100 {
		current := b.reserveThread("C", "T")
		later := b.reserveThread("C", "T")
		var next *threadReservation
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); next, _ = b.reserveThreadSuccessor("C", "T", current) }()
		go func() { defer wg.Done(); b.releaseThreadReservation("C", "T", current) }()
		wg.Wait()
		if next != nil {
			next.Wait()
			assertChainBlocked(t, later.ready)
			b.releaseThreadReservation("C", "T", next)
		}
		waitChainSignal(t, later.ready)
		b.releaseThreadReservation("C", "T", later)
	}
	if len(b.threadLocks) != 0 {
		t.Fatal("FIFO references leaked")
	}
}

type chainTurn struct {
	opts   agent.OneShotOpts
	finish chan struct{}
}
type chainManager struct {
	mockMgr
	calls chan chainTurn
}

func (m *chainManager) SteerOneShot(context.Context, string, string, string) error { return nil }

func (m *chainManager) ChatOneShot(ctx context.Context, _, _ string, opts agent.OneShotOpts) (<-chan agent.ChatEvent, error) {
	call := chainTurn{opts: opts, finish: make(chan struct{})}
	m.calls <- call
	events := make(chan agent.ChatEvent, 1)
	go func() {
		defer close(events)
		select {
		case <-call.finish:
			events <- agent.ChatEvent{Type: "done", Message: &agent.Message{Content: "finished"}}
		case <-ctx.Done():
		}
	}()
	return events, nil
}
func nextChainTurn(t *testing.T, m *chainManager) chainTurn {
	t.Helper()
	select {
	case c := <-m.calls:
		return c
	case <-time.After(3 * time.Second):
		t.Fatal("Slack continuation did not start")
		return chainTurn{}
	}
}
func chainReservation(t *testing.T, c chainTurn) *slackHandoffReservation {
	t.Helper()
	r, ok := c.opts.HandoffArrivalReservation.(*slackHandoffReservation)
	if !ok || r == nil {
		t.Fatal("missing concrete handoff reservation")
	}
	return r
}

// Exercise the actual Activate goroutine, synthetic turn, ChatOneShot options,
// terminal Slack/history delivery and FIFO release on two consecutive hops.
func TestSlackHandoffChainKeepsSameThreadAheadOfQueuedHuman(t *testing.T) {
	b := newTestBot(t, agent.SlackBotConfig{})
	defer b.cancel()
	m := &chainManager{calls: make(chan chainTurn, 8)}
	b.mgr = m
	sourceDone := make(chan struct{})
	go func() { defer close(sourceDone); b.sendToAgent(b.ctx, "C1", "T1", "T1", "1.0", "move", "Alice", "U1") }()
	first := nextChainTurn(t, m)
	firstR := chainReservation(t, first)
	// Admit a human turn now, deterministically behind the original pair.
	ordinary, ordinaryArrival := b.reserveThreadPair("C1", "T1")
	ctx, cancel := context.WithCancel(b.ctx)
	active := b.registerActiveTurnForUser("C1", "T1", "U2", cancel)
	humanDone := make(chan struct{})
	go func() {
		defer close(humanDone)
		b.sendToAgentTurnReserved(b.ctx, "C1", "T1", "T1", "2.0", "later", "Bob", "U2", "", nil, false, "2.0", nil, ordinary, ordinaryArrival, ctx, cancel, active)
	}()
	if err := firstR.Activate(b.ctx, "arrived B", "peer-B"); err != nil {
		t.Fatal(err)
	}
	if err := firstR.Activate(b.ctx, "duplicate", "peer-B"); err != nil {
		t.Fatal(err)
	}
	close(first.finish)
	second := nextChainTurn(t, m)
	secondR := chainReservation(t, second)
	// A steer accepted by the synthetic source must follow the next handoff.
	b.userCache["U1"] = "Alice"
	b.processIncoming(b.ctx, "C1", "T1", "1.5", "carry second-hop steer", "U1")
	waitSteerQueue(t, secondR.source)
	if second.opts.SessionKey != first.opts.SessionKey || !second.opts.ForceFreshSession || second.opts.ExpectedHolderPeer != "peer-B" || second.opts.GoalUserID != "U1" {
		t.Fatalf("bad first arrival: %+v", second.opts)
	}
	assertChainBlocked(t, ordinary.ready)
	if err := secondR.Activate(b.ctx, "arrived A", "peer-A"); err != nil {
		t.Fatal(err)
	}
	if err := secondR.Activate(b.ctx, "duplicate", "peer-A"); err != nil {
		t.Fatal(err)
	}
	// Active registry and FIFO must both place the next arrival before U2.
	b.activeTurnsMu.Lock()
	turns := append([]*activeTurn(nil), b.activeTurns[activeTurnKey("C1", "T1")]...)
	b.activeTurnsMu.Unlock()
	if len(turns) != 3 || turns[0] != secondR.source || turns[1].ownerUserID != "U1" || turns[2] != active {
		t.Fatal("active registry diverged from continuation FIFO")
	}
	close(second.finish)
	third := nextChainTurn(t, m)
	thirdR := chainReservation(t, third)
	if third.opts.SessionKey != first.opts.SessionKey || !third.opts.ForceFreshSession || third.opts.ExpectedHolderPeer != "peer-A" || thirdR.userID != "U1" {
		t.Fatalf("bad return arrival: %+v", third.opts)
	}
	found, foundSteer := false, false
	for _, h := range third.opts.History {
		if h.MessageID == "1.0" && h.UserID == "U1" {
			found = true
		}
		if h.MessageID == "2.0" {
			t.Fatal("later human leaked into handoff history")
		}
		if h.MessageID == "1.5" && h.Text == "carry second-hop steer" {
			foundSteer = true
		}
	}
	if !found {
		t.Fatal("source history lost across two hops")
	}
	if !foundSteer {
		t.Fatal("second-hop steer lost")
	}
	assertChainBlocked(t, ordinary.ready)
	close(third.finish) // unused successor must be released
	human := nextChainTurn(t, m)
	if human.opts.GoalUserID != "U2" || human.opts.ForceFreshSession {
		t.Fatal("expected next ordinary user")
	}
	close(human.finish)
	waitChainSignal(t, sourceDone)
	waitChainSignal(t, humanDone)
	for _, r := range []*slackHandoffReservation{firstR, secondR, thirdR} {
		ctx, cancel := context.WithTimeout(b.ctx, time.Second)
		err := r.WaitSourceComplete(ctx)
		cancel()
		if err != nil {
			t.Fatalf("source completion barrier: %v", err)
		}
	}
	b.threadLocksMu.Lock()
	remaining := len(b.threadLocks)
	b.threadLocksMu.Unlock()
	if remaining != 0 {
		t.Fatal("chain or unused successor leaked")
	}
	select {
	case <-m.calls:
		t.Fatal("duplicate activation started an extra turn")
	default:
	}
}

func TestSlackNilHandoffReservationFailsWithoutPanic(t *testing.T) {
	var r *slackHandoffReservation
	if err := r.Activate(context.Background(), "arrived", "hub"); err == nil {
		t.Fatal("nil arrival accepted")
	}
	if err := r.WaitSourceComplete(context.Background()); err == nil {
		t.Fatal("nil barrier accepted")
	}
	r.Release()
}

func TestSplitHandoffCancellationReleasesFIFOAndStopRegistry(t *testing.T) {
	for _, mode := range []string{"unused", "source-stopped", "successor-stopped-with-full-semaphore"} {
		t.Run(mode, func(t *testing.T) {
			b := newTestBot(t, agent.SlackBotConfig{})
			defer b.cancel()
			m := &countingMgr{}
			b.mgr = m
			current := b.reserveThread("C", "T")
			next, err := b.reserveThreadSuccessor("C", "T", current)
			if err != nil {
				t.Fatal(err)
			}
			later := b.reserveThread("C", "T")
			_, cancel := context.WithCancel(b.ctx)
			source := b.registerActiveTurnForUser("C", "T", "U1", cancel)
			_, cancelLater := context.WithCancel(b.ctx)
			defer cancelLater()
			human := b.registerActiveTurnForUser("C", "T", "U2", cancelLater)
			r := &slackHandoffReservation{bot: b, channel: "C", threadTS: "T", source: source, reservation: next, userID: "U1"}
			for range cap(b.sem) {
				b.sem <- struct{}{}
			}
			if mode != "unused" {
				if err := r.Activate(b.ctx, "arrived", "hub"); err != nil {
					t.Fatal(err)
				}
			}
			if mode != "successor-stopped-with-full-semaphore" {
				source.requestStop()
			}
			b.finishActiveTurn("C", "T", source)
			cancel()
			b.releaseThreadReservation("C", "T", current)
			if mode == "unused" {
				r.Release()
			}
			if mode == "successor-stopped-with-full-semaphore" {
				_, started, denied := b.cancelActiveTurnForCommand("C", "T", "U2")
				if started || !denied {
					t.Fatal("different user stopped reserved arrival")
				}
				turn, started, denied := b.cancelActiveTurnForCommand("C", "T", "U1")
				if turn == nil || !started || denied {
					t.Fatal("arrival was not stoppable behind semaphore")
				}
				turn.completeStopAck()
			}
			waitChainSignal(t, later.ready)
			if m.oneShots.Load() != 0 {
				t.Fatal("discarded successor started a chat")
			}
			b.activeTurnsMu.Lock()
			turns := b.activeTurns[activeTurnKey("C", "T")]
			clean := len(turns) == 1 && turns[0] == human && len(b.stoppingTurns) == 0
			b.activeTurnsMu.Unlock()
			if !clean {
				t.Fatal("active/stop registry leaked or reordered")
			}
			b.finishActiveTurn("C", "T", human)
			b.releaseThreadReservation("C", "T", later)
			b.threadLocksMu.Lock()
			left := len(b.threadLocks)
			b.threadLocksMu.Unlock()
			if left != 0 {
				t.Fatal("split FIFO leaked")
			}
		})
	}
}

func TestSplitHandoffSourceBarrierDoesNotWaitForSuccessor(t *testing.T) {
	b := newTestBot(t, agent.SlackBotConfig{})
	defer b.cancel()
	current := b.reserveThread("C", "T")
	next, err := b.reserveThreadSuccessor("C", "T", current)
	if err != nil {
		t.Fatal(err)
	}
	later := b.reserveThread("C", "T")
	r := &slackHandoffReservation{reservation: next}
	ctx, cancel := context.WithTimeout(b.ctx, 10*time.Millisecond)
	if err := r.WaitSourceComplete(ctx); err != context.DeadlineExceeded {
		t.Fatalf("early barrier: %v", err)
	}
	cancel()
	b.releaseThreadReservation("C", "T", current)
	ctx, cancel = context.WithTimeout(b.ctx, time.Second)
	defer cancel()
	if err := r.WaitSourceComplete(ctx); err != nil {
		t.Fatal(err)
	}
	assertChainBlocked(t, later.ready)
	b.releaseThreadReservation("C", "T", next)
	later.Wait()
	b.releaseThreadReservation("C", "T", later)
}
