package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/state"
	"github.com/Dragonshorn-Studios/yukariko/internal/store"
)

// streamPassEvents mirrors new store events for one app to the terminal
// while a manually triggered pass runs: real stage names (preflight,
// deploying, postchecks) with elapsed time, so a multi-minute first build
// shows progress without streaming un-redacted command output. finished is
// closed when the pass ends (the result itself travels on a separate
// channel); the events remain in `yukariko logs`.
func streamPassEvents(ctx context.Context, out io.Writer, st *store.Store, appID string, finished <-chan struct{}, start time.Time) {
	last := start
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-finished:
			printNewEvents(ctx, out, st, appID, &last, start)
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			printNewEvents(ctx, out, st, appID, &last, start)
		}
	}
}

func printNewEvents(ctx context.Context, out io.Writer, st *store.Store, appID string, last *time.Time, start time.Time) {
	events, err := state.EventsFor(ctx, st, appID, "", 50, time.Since(*last))
	if err != nil {
		return // polling is best effort
	}
	// EventsFor returns newest-first; print chronologically.
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if !e.Time.After(*last) {
			continue
		}
		fmt.Fprintf(out, "  [%s] %s\n", formatElapsed(time.Since(start)), e.Message)
		*last = e.Time
	}
}

func formatElapsed(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}
