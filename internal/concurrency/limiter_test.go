package concurrency

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"
)

type acquireResult struct {
	release func()
	err     error
}

func testLimiter(t *testing.T, limit, maxQueue int) *Limiter {
	t.Helper()
	limiter, err := New(Config{
		Enabled:              true,
		GlobalLimit:          limit,
		MaxQueuePerPrincipal: maxQueue,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return limiter
}

func mustAcquire(t *testing.T, limiter *Limiter, principal string) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	release, err := limiter.Acquire(ctx, principal)
	if err != nil {
		t.Fatalf("Acquire(%q) error = %v", principal, err)
	}
	return release
}

func startAcquire(limiter *Limiter, principal string) (context.CancelFunc, <-chan acquireResult) {
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan acquireResult, 1)
	go func() {
		release, err := limiter.Acquire(ctx, principal)
		result <- acquireResult{release: release, err: err}
	}()
	return cancel, result
}

func waitForStats(t *testing.T, limiter *Limiter, predicate func(Stats) bool) Stats {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stats := limiter.Stats()
		if predicate(stats) {
			return stats
		}
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
	stats := limiter.Stats()
	t.Fatalf("stats predicate timed out: %+v", stats)
	return stats
}

func principalStats(stats Stats, principal string) PrincipalStats {
	for _, item := range stats.Principals {
		if item.Principal == principal {
			return item
		}
	}
	return PrincipalStats{Principal: principal}
}

func TestSingleUserCanUseAllConcurrency(t *testing.T) {
	limiter := testLimiter(t, 6, 32)
	releases := make([]func(), 0, 6)
	for i := 0; i < 6; i++ {
		releases = append(releases, mustAcquire(t, limiter, "A"))
	}
	stats := limiter.Stats()
	if stats.GlobalRunning != 6 || principalStats(stats, "A").Running != 6 {
		t.Fatalf("stats = %+v, want A running all 6 slots", stats)
	}
	for _, release := range releases {
		release()
	}
}

func TestSecondUserGetsReleasedSlots(t *testing.T) {
	limiter := testLimiter(t, 6, 32)
	aReleases := make([]func(), 0, 6)
	for i := 0; i < 6; i++ {
		aReleases = append(aReleases, mustAcquire(t, limiter, "A"))
	}
	cancelB, bResult := startAcquire(limiter, "B")
	defer cancelB()
	waitForStats(t, limiter, func(stats Stats) bool { return principalStats(stats, "B").Waiting == 1 })

	aReleases[0]()
	b := <-bResult
	if b.err != nil {
		t.Fatalf("B Acquire() error = %v", b.err)
	}
	stats := limiter.Stats()
	if principalStats(stats, "A").Running != 5 || principalStats(stats, "B").Running != 1 {
		t.Fatalf("stats after A release = %+v, want A=5 B=1", stats)
	}
	b.release()
	for _, release := range aReleases[1:] {
		release()
	}
}

func TestFairConvergenceTwoUsers(t *testing.T) {
	limiter := testLimiter(t, 6, 32)
	initial := make([]func(), 0, 6)
	for i := 0; i < 6; i++ {
		initial = append(initial, mustAcquire(t, limiter, "A"))
	}

	var cancels []context.CancelFunc
	var results []<-chan acquireResult
	for _, principal := range []string{"A", "B"} {
		for i := 0; i < 6; i++ {
			cancel, result := startAcquire(limiter, principal)
			cancels = append(cancels, cancel)
			results = append(results, result)
		}
	}
	defer func() {
		for _, cancel := range cancels {
			cancel()
		}
		for _, result := range results {
			select {
			case got := <-result:
				if got.err == nil && got.release != nil {
					got.release()
				}
			default:
			}
		}
	}()
	waitForStats(t, limiter, func(stats Stats) bool { return stats.TotalWaiting == 12 })
	for _, release := range initial {
		release()
	}
	stats := waitForStats(t, limiter, func(stats Stats) bool {
		return principalStats(stats, "A").Running == 3 && principalStats(stats, "B").Running == 3
	})
	if stats.GlobalRunning != 6 {
		t.Fatalf("stats = %+v, want global running 6", stats)
	}
}

func TestFairConvergenceThreeUsers(t *testing.T) {
	limiter := testLimiter(t, 6, 32)
	initial := make([]func(), 0, 6)
	for i := 0; i < 6; i++ {
		initial = append(initial, mustAcquire(t, limiter, "A"))
	}

	var cancels []context.CancelFunc
	var results []<-chan acquireResult
	for _, principal := range []string{"A", "B", "C"} {
		for i := 0; i < 6; i++ {
			cancel, result := startAcquire(limiter, principal)
			cancels = append(cancels, cancel)
			results = append(results, result)
		}
	}
	defer func() {
		for _, cancel := range cancels {
			cancel()
		}
		for _, result := range results {
			select {
			case got := <-result:
				if got.err == nil && got.release != nil {
					got.release()
				}
			default:
			}
		}
	}()
	waitForStats(t, limiter, func(stats Stats) bool { return stats.TotalWaiting == 18 })
	for _, release := range initial {
		release()
	}
	waitForStats(t, limiter, func(stats Stats) bool {
		return principalStats(stats, "A").Running == 2 &&
			principalStats(stats, "B").Running == 2 &&
			principalStats(stats, "C").Running == 2
	})
}

func TestIdleCapacityCanBeBorrowed(t *testing.T) {
	limiter := testLimiter(t, 6, 32)
	for i := 0; i < 6; i++ {
		_ = mustAcquire(t, limiter, "A")
	}
	if got := principalStats(limiter.Stats(), "A").Running; got != 6 {
		t.Fatalf("A running = %d, want 6", got)
	}
}

func TestPerUserFIFO(t *testing.T) {
	limiter := testLimiter(t, 1, 32)
	holder := mustAcquire(t, limiter, "A")

	type identifiedResult struct {
		id      int
		release func()
		err     error
	}
	results := make(chan identifiedResult, 3)
	cancels := make([]context.CancelFunc, 0, 3)
	for id := 1; id <= 3; id++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancels = append(cancels, cancel)
		go func(id int) {
			release, err := limiter.Acquire(ctx, "A")
			results <- identifiedResult{id: id, release: release, err: err}
		}(id)
		wantWaiting := id
		waitForStats(t, limiter, func(stats Stats) bool { return principalStats(stats, "A").Waiting == wantWaiting })
	}
	defer func() {
		for _, cancel := range cancels {
			cancel()
		}
	}()

	holder()
	for want := 1; want <= 3; want++ {
		got := <-results
		if got.err != nil || got.id != want {
			t.Fatalf("admission %d = id %d err %v, want FIFO id %d", want, got.id, got.err, want)
		}
		got.release()
	}
}

func TestNoStarvation(t *testing.T) {
	limiter := testLimiter(t, 1, 32)
	holder := mustAcquire(t, limiter, "A")

	var cancelA []context.CancelFunc
	var aResults []<-chan acquireResult
	for i := 0; i < 10; i++ {
		cancel, result := startAcquire(limiter, "A")
		cancelA = append(cancelA, cancel)
		aResults = append(aResults, result)
	}
	waitForStats(t, limiter, func(stats Stats) bool { return principalStats(stats, "A").Waiting == 10 })
	cancelB, bResult := startAcquire(limiter, "B")
	defer cancelB()
	waitForStats(t, limiter, func(stats Stats) bool { return principalStats(stats, "B").Waiting == 1 })

	holder()
	select {
	case got := <-bResult:
		if got.err != nil {
			t.Fatalf("B Acquire() error = %v", got.err)
		}
		got.release()
	case <-time.After(time.Second):
		t.Fatal("B starved behind A's queued backlog")
	}
	for _, cancel := range cancelA {
		cancel()
	}
	for _, result := range aResults {
		got := <-result
		if got.err == nil && got.release != nil {
			got.release()
		}
	}
}

func TestQueueTimeout(t *testing.T) {
	limiter := testLimiter(t, 1, 32)
	holder := mustAcquire(t, limiter, "A")
	defer holder()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := limiter.Acquire(ctx, "B")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Acquire() error = %v, want deadline exceeded", err)
	}
	stats := limiter.Stats()
	if stats.TotalWaiting != 0 || principalStats(stats, "B").Waiting != 0 {
		t.Fatalf("timed-out waiter remains queued: %+v", stats)
	}
}

func TestContextCancel(t *testing.T) {
	limiter := testLimiter(t, 1, 32)
	holder := mustAcquire(t, limiter, "A")
	defer holder()

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := limiter.Acquire(ctx, "B")
		result <- err
	}()
	waitForStats(t, limiter, func(stats Stats) bool { return principalStats(stats, "B").Waiting == 1 })
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire() error = %v, want context canceled", err)
	}
	if stats := limiter.Stats(); stats.TotalWaiting != 0 {
		t.Fatalf("canceled waiter remains queued: %+v", stats)
	}
}

func TestMaxQueuePerKey(t *testing.T) {
	limiter := testLimiter(t, 1, 1)
	holder := mustAcquire(t, limiter, "A")
	defer holder()
	cancel, first := startAcquire(limiter, "A")
	defer cancel()
	waitForStats(t, limiter, func(stats Stats) bool { return principalStats(stats, "A").Waiting == 1 })

	ctx, cancelSecond := context.WithTimeout(context.Background(), time.Second)
	defer cancelSecond()
	if _, err := limiter.Acquire(ctx, "A"); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("second queued Acquire() error = %v, want ErrQueueFull", err)
	}
	cancel()
	if got := <-first; !errors.Is(got.err, context.Canceled) {
		t.Fatalf("first queued Acquire() error = %v, want canceled cleanup", got.err)
	}
}

func TestReleaseExactlyOnce(t *testing.T) {
	limiter := testLimiter(t, 1, 32)
	aRelease := mustAcquire(t, limiter, "A")
	cancelB, bResult := startAcquire(limiter, "B")
	defer cancelB()
	waitForStats(t, limiter, func(stats Stats) bool { return stats.TotalWaiting == 1 })

	aRelease()
	aRelease()
	b := <-bResult
	if b.err != nil {
		t.Fatalf("B Acquire() error = %v", b.err)
	}
	stats := limiter.Stats()
	if stats.GlobalRunning != 1 || principalStats(stats, "B").Running != 1 {
		t.Fatalf("double release corrupted counts: %+v", stats)
	}
	b.release()
	b.release()
	if stats = limiter.Stats(); stats.GlobalRunning != 0 {
		t.Fatalf("final stats = %+v, want zero running", stats)
	}
}

func TestDisabledLimiter(t *testing.T) {
	limiter, err := New(Config{Enabled: false})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	for i := 0; i < 100; i++ {
		release, acquireErr := limiter.Acquire(context.Background(), "A")
		if acquireErr != nil {
			t.Fatalf("disabled Acquire() error = %v", acquireErr)
		}
		release()
	}
	stats := limiter.Stats()
	if stats.GlobalRunning != 0 || stats.TotalWaiting != 0 {
		t.Fatalf("disabled limiter tracked requests: %+v", stats)
	}
}

func TestConcurrentRace(t *testing.T) {
	limiter := testLimiter(t, 8, 256)
	var wg sync.WaitGroup
	for i := 0; i < 256; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			principal := string(rune('A' + i%8))
			release, err := limiter.Acquire(ctx, principal)
			if err != nil {
				return
			}
			runtime.Gosched()
			release()
			release()
		}(i)
	}
	wg.Wait()
	stats := limiter.Stats()
	if stats.GlobalRunning != 0 || stats.TotalWaiting != 0 {
		t.Fatalf("stats after concurrent run = %+v, want empty", stats)
	}
}

func TestReconfigureLowerLimitIsNonPreemptive(t *testing.T) {
	limiter := testLimiter(t, 2, 32)
	releaseA := mustAcquire(t, limiter, "A")
	releaseB := mustAcquire(t, limiter, "B")
	if err := limiter.Reconfigure(Config{Enabled: true, GlobalLimit: 1, MaxQueuePerPrincipal: 32}); err != nil {
		t.Fatalf("Reconfigure() error = %v", err)
	}
	cancelC, cResult := startAcquire(limiter, "C")
	defer cancelC()
	waitForStats(t, limiter, func(stats Stats) bool { return principalStats(stats, "C").Waiting == 1 })
	releaseA()
	select {
	case got := <-cResult:
		if got.err == nil {
			got.release()
		}
		t.Fatal("C was admitted while running still equaled lowered limit")
	case <-time.After(20 * time.Millisecond):
	}
	releaseB()
	c := <-cResult
	if c.err != nil {
		t.Fatalf("C Acquire() error = %v", c.err)
	}
	c.release()
}
