package cull

import (
	"errors"
	"log"
	"sync"
	"time"

	"github.com/zack/fuji-tools/internal/synccore"
)

// syncer drives the engine side of cross-device sync: push the outbox, pull
// deltas, reconcile a server generation change. It is offline-tolerant (retry
// forever with escalating backoff, reset on success) and event-driven (a local
// decision nudges it). It never caches a session pointer — provide() re-reads
// the current session + camera slug under the app lock every cycle, because the
// session pointer is swapped live when the camera re-keys.
type syncer struct {
	client  *syncClient
	provide func() (sess *Session, camera string) // reads under app.mu
	wake    chan struct{}
	stop    chan struct{}
	done    chan struct{}

	sm       sync.Mutex
	lastOkMs int64
	lastErr  string
}

// status is a snapshot for /api/status.sync.
type syncStatusSnapshot struct {
	lastOkMs int64
	lastErr  string
}

func (s *syncer) status() syncStatusSnapshot {
	s.sm.Lock()
	defer s.sm.Unlock()
	return syncStatusSnapshot{lastOkMs: s.lastOkMs, lastErr: s.lastErr}
}

func (s *syncer) note(err error) {
	s.sm.Lock()
	if err == nil {
		s.lastOkMs = nowMs()
		s.lastErr = ""
	} else {
		s.lastErr = err.Error()
	}
	s.sm.Unlock()
}

func newSyncer(client *syncClient, provide func() (*Session, string)) *syncer {
	return &syncer{
		client:  client,
		provide: provide,
		wake:    make(chan struct{}, 1),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
}

// Nudge asks the syncer to run a cycle soon (non-blocking; coalesces).
func (s *syncer) Nudge() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Stop ends the loop and waits for it to exit.
func (s *syncer) Stop() {
	close(s.stop)
	<-s.done
}

var syncBackoff = []time.Duration{5 * time.Second, 15 * time.Second, 45 * time.Second, 2 * time.Minute}

func backoffAt(n int) time.Duration {
	if n >= len(syncBackoff) {
		return syncBackoff[len(syncBackoff)-1]
	}
	return syncBackoff[n]
}

func (s *syncer) Run() {
	defer close(s.done)
	fails := 0
	// idle re-poll cadence when there's nothing to push (still catch remote
	// changes); short first tick so the initial sync happens promptly.
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-s.wake:
		case <-timer.C:
		}
		sess, camera := s.provide()
		var wait time.Duration
		if sess == nil || camera == "" {
			wait = 5 * time.Second // no identity yet — wait for discovery
		} else if err := syncOnce(s.client, sess, camera); err != nil {
			s.note(err)
			if errors.Is(err, errAuth) {
				log.Printf("sync: %v — pausing until reconfigured", err)
				wait = time.Hour // sticky: don't hammer a bad key
			} else {
				wait = backoffAt(fails)
				fails++
			}
		} else {
			s.note(nil)
			fails = 0
			wait = 30 * time.Second // steady-state idle re-poll
		}
		resetTimer(timer, wait)
	}
}

// syncOnce runs a single push-then-pull reconcile for one camera. It is the unit
// the loop repeats and the integration test drives directly.
func syncOnce(client *syncClient, sess *Session, camera string) error {
	// PUSH the outbox
	out := sess.Outbox()
	serverVer, epoch, dev := sess.SyncMeta()
	if len(out) > 0 {
		resp, err := client.push(synccore.PushRequest{
			Camera: camera, DeviceID: dev, Epoch: epoch, Decisions: out,
		})
		if err != nil {
			return err
		}
		if epoch != "" && resp.Epoch != epoch {
			// server generation changed under us — reseed on the next cycle
			sess.ResetForEpoch(resp.Epoch)
			return nil
		}
		// adopt the server's winners (incl. rejected pushes) so a losing device
		// converges to the server's state, and clean acked records
		sess.AckPush(resp)
	}

	// PULL deltas since our high-water
	serverVer, epoch, _ = sess.SyncMeta()
	pr, err := client.pull(camera, serverVer)
	if err != nil {
		return err
	}
	if epoch != "" && pr.Epoch != epoch {
		sess.ResetForEpoch(pr.Epoch)
		return nil
	}
	// client-side rewind detector: a server high-water below ours means the DB
	// was restored stale even if the epoch somehow matched — reseed.
	if pr.DeltaHigh < serverVer {
		sess.ResetForEpoch(pr.Epoch)
		return nil
	}
	sess.ApplyRemote(pr.Decisions, pr.Resume, pr.Epoch, pr.DeltaHigh, pr.ServerNow)
	return nil
}

func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}
