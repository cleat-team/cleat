package main

import (
	"log/slog"
	"time"

	"github.com/lib/pq"
)

// startNotifyListener opens a dedicated connection for PostgreSQL LISTEN and
// forwards notifications to notifyCh. It returns a close function that stops
// the listener and waits for the goroutine to exit.
//
// When dsn is empty or channel is empty, it returns a no-op close function and
// notifyCh remains nil — the dispatch loop's nil-channel select case safely
// blocks forever.
func startNotifyListener(dsn, channel string, notifyCh chan<- struct{}, logger *slog.Logger) func() {
	if dsn == "" || channel == "" {
		return func() {}
	}

	report := func(ev pq.ListenerEventType, err error) {
		if err != nil {
			logger.Warn("pg notify listener event", "event", ev, "error", err)
		}
	}
	listener := pq.NewListener(dsn, 10*time.Second, 30*time.Second, report)
	if err := listener.Listen(channel); err != nil {
		logger.Warn("pg notify listen failed, dispatch will rely on polling", "channel", channel, "error", err)
		listener.Close()
		return func() {}
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for n := range listener.Notify {
			if n == nil {
				return
			}
			select {
			case notifyCh <- struct{}{}:
			default:
				// Already pending — don't block.
			}
		}
	}()

	return func() {
		listener.Close()
		<-done
	}
}
