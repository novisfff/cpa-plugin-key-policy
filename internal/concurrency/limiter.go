// Package concurrency implements a process-local, non-preemptive fair
// concurrency limiter. It is independent of CLIProxyAPI so the scheduling
// behavior can be exercised with ordinary unit tests.
package concurrency

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
)

var (
	ErrInvalidPrincipal = errors.New("concurrency principal is required")
	ErrInvalidLimit     = errors.New("global concurrency limit must be positive")
	ErrInvalidMaxQueue  = errors.New("max queue per principal must be positive")
	ErrQueueFull        = errors.New("concurrency queue is full for principal")
)

type Config struct {
	Enabled              bool
	GlobalLimit          int
	MaxQueuePerPrincipal int
}

type PrincipalStats struct {
	Principal string `json:"principal"`
	Running   int    `json:"running"`
	Waiting   int    `json:"waiting"`
}

type Stats struct {
	Enabled          bool             `json:"enabled"`
	GlobalLimit      int              `json:"global_limit"`
	GlobalRunning    int              `json:"global_running"`
	TotalWaiting     int              `json:"total_waiting"`
	ActivePrincipals int              `json:"active_principals"`
	Principals       []PrincipalStats `json:"principals"`
}

type waiterState uint8

const (
	waiterWaiting waiterState = iota
	waiterAdmitted
	waiterCanceled
)

type waiter struct {
	sequence uint64
	ready    chan struct{}
	state    waiterState
	counted  bool
}

type principalState struct {
	running   int
	waiting   []*waiter
	lastGrant uint64
}

type Limiter struct {
	mu sync.Mutex

	config        Config
	globalRunning int
	totalWaiting  int
	waitSequence  uint64
	grantSequence uint64
	principals    map[string]*principalState
}

func New(config Config) (*Limiter, error) {
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	return &Limiter{
		config:     config,
		principals: make(map[string]*principalState),
	}, nil
}

func validateConfig(config Config) error {
	if !config.Enabled {
		return nil
	}
	if config.GlobalLimit <= 0 {
		return ErrInvalidLimit
	}
	if config.MaxQueuePerPrincipal <= 0 {
		return ErrInvalidMaxQueue
	}
	return nil
}

func (l *Limiter) Acquire(ctx context.Context, principal string) (func(), error) {
	principal = strings.TrimSpace(principal)
	if principal == "" {
		return nil, ErrInvalidPrincipal
	}
	if ctx == nil {
		ctx = context.Background()
	}

	l.mu.Lock()
	if !l.config.Enabled {
		l.mu.Unlock()
		return noOpRelease(), nil
	}
	state := l.principals[principal]
	if state == nil {
		state = &principalState{}
		l.principals[principal] = state
	}
	if l.globalRunning < l.config.GlobalLimit && l.totalWaiting == 0 {
		l.admitDirectLocked(state)
		l.mu.Unlock()
		return l.releaseFunc(principal, true), nil
	}
	if len(state.waiting) >= l.config.MaxQueuePerPrincipal {
		l.cleanupPrincipalLocked(principal, state)
		l.mu.Unlock()
		return nil, ErrQueueFull
	}
	l.waitSequence++
	w := &waiter{
		sequence: l.waitSequence,
		ready:    make(chan struct{}),
		state:    waiterWaiting,
	}
	state.waiting = append(state.waiting, w)
	l.totalWaiting++
	l.scheduleLocked()
	l.mu.Unlock()

	select {
	case <-w.ready:
		return l.releaseForWaiter(principal, w), nil
	case <-ctx.Done():
		l.mu.Lock()
		if w.state == waiterAdmitted {
			counted := w.counted
			l.mu.Unlock()
			return l.releaseFunc(principal, counted), nil
		}
		if w.state == waiterWaiting {
			l.removeWaiterLocked(principal, state, w)
			w.state = waiterCanceled
		}
		l.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (l *Limiter) releaseForWaiter(principal string, w *waiter) func() {
	l.mu.Lock()
	counted := w.counted
	l.mu.Unlock()
	return l.releaseFunc(principal, counted)
}

func noOpRelease() func() {
	var once sync.Once
	return func() { once.Do(func() {}) }
}

func (l *Limiter) releaseFunc(principal string, counted bool) func() {
	if !counted {
		return noOpRelease()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			l.release(principal)
		})
	}
}

func (l *Limiter) release(principal string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	state := l.principals[principal]
	if state == nil || state.running <= 0 || l.globalRunning <= 0 {
		return
	}
	state.running--
	l.globalRunning--
	l.scheduleLocked()
	l.cleanupPrincipalLocked(principal, state)
}

func (l *Limiter) admitDirectLocked(state *principalState) {
	l.globalRunning++
	state.running++
	l.grantSequence++
	state.lastGrant = l.grantSequence
}

func (l *Limiter) scheduleLocked() {
	if !l.config.Enabled {
		return
	}
	for l.globalRunning < l.config.GlobalLimit && l.totalWaiting > 0 {
		principal, state := l.nextPrincipalLocked()
		if state == nil || len(state.waiting) == 0 {
			return
		}
		w := state.waiting[0]
		state.waiting[0] = nil
		state.waiting = state.waiting[1:]
		l.totalWaiting--
		l.globalRunning++
		state.running++
		l.grantSequence++
		state.lastGrant = l.grantSequence
		w.state = waiterAdmitted
		w.counted = true
		close(w.ready)
		_ = principal
	}
}

func (l *Limiter) nextPrincipalLocked() (string, *principalState) {
	var selectedName string
	var selected *principalState
	for name, state := range l.principals {
		if state == nil || len(state.waiting) == 0 {
			continue
		}
		if selected == nil || state.running < selected.running ||
			(state.running == selected.running && state.lastGrant < selected.lastGrant) ||
			(state.running == selected.running && state.lastGrant == selected.lastGrant &&
				state.waiting[0].sequence < selected.waiting[0].sequence) {
			selectedName = name
			selected = state
		}
	}
	return selectedName, selected
}

func (l *Limiter) removeWaiterLocked(principal string, state *principalState, target *waiter) {
	for index, candidate := range state.waiting {
		if candidate != target {
			continue
		}
		copy(state.waiting[index:], state.waiting[index+1:])
		state.waiting[len(state.waiting)-1] = nil
		state.waiting = state.waiting[:len(state.waiting)-1]
		l.totalWaiting--
		break
	}
	l.cleanupPrincipalLocked(principal, state)
}

func (l *Limiter) cleanupPrincipalLocked(principal string, state *principalState) {
	if state != nil && state.running == 0 && len(state.waiting) == 0 {
		delete(l.principals, principal)
	}
}

func (l *Limiter) Reconfigure(config Config) error {
	if err := validateConfig(config); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	wasEnabled := l.config.Enabled
	l.config = config
	if wasEnabled && !config.Enabled {
		for principal, state := range l.principals {
			for _, w := range state.waiting {
				w.state = waiterAdmitted
				w.counted = false
				close(w.ready)
			}
			state.waiting = nil
			if state.running == 0 {
				delete(l.principals, principal)
			}
		}
		l.totalWaiting = 0
		return nil
	}
	l.scheduleLocked()
	return nil
}

func (l *Limiter) Stats() Stats {
	l.mu.Lock()
	defer l.mu.Unlock()
	stats := Stats{
		Enabled:       l.config.Enabled,
		GlobalLimit:   l.config.GlobalLimit,
		GlobalRunning: l.globalRunning,
		TotalWaiting:  l.totalWaiting,
		Principals:    make([]PrincipalStats, 0, len(l.principals)),
	}
	for principal, state := range l.principals {
		if state == nil || (state.running == 0 && len(state.waiting) == 0) {
			continue
		}
		stats.Principals = append(stats.Principals, PrincipalStats{
			Principal: principal,
			Running:   state.running,
			Waiting:   len(state.waiting),
		})
	}
	sort.Slice(stats.Principals, func(i, j int) bool {
		return stats.Principals[i].Principal < stats.Principals[j].Principal
	})
	stats.ActivePrincipals = len(stats.Principals)
	return stats
}
