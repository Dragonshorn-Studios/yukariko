package schedule

import (
	"context"
	"fmt"
	"time"
)

// State is one app's position in the deployment state machine. Transitions
// are validated and recorded as events; an invalid transition is never
// applied.
type State string

const (
	StateIdle        State = "idle"
	StateChecking    State = "checking"
	StatePreflight   State = "preflight"
	StateDeploying   State = "deploying"
	StatePostchecks  State = "postchecks" // wired by #13 into the deploy pipeline
	StateSucceeded   State = "succeeded"
	StateFailed      State = "failed"
	StateBackoff     State = "backoff"
	StateInterrupted State = "interrupted"
)

// Event kinds recorded alongside state transitions.
const (
	EventState             = "state"
	EventSkipLocked        = "skip_locked"
	EventPreflightPassed   = "preflight_passed"
	EventPreflightRefused  = "preflight_refused"
	EventManualTrigger     = "manual_trigger"
	EventInvalidTransition = "invalid_transition"
)

// allowedTransitions is the explicit state machine. Outcomes return to the
// working flow: succeeded waits idle-until the next tick, failed always
// chains to backoff, and backoff ends when its timer releases the next
// checking pass.
var allowedTransitions = map[State]map[State]bool{
	StateIdle: {StateChecking: true},
	StateChecking: {
		StateSucceeded: true, StatePreflight: true,
		StateFailed: true, StateInterrupted: true,
	},
	StatePreflight: {
		StateDeploying: true, StateFailed: true, StateInterrupted: true,
	},
	StateDeploying: {
		StatePostchecks: true, StateSucceeded: true,
		StateFailed: true, StateInterrupted: true,
	},
	StatePostchecks: {
		StateSucceeded: true, StateFailed: true, StateInterrupted: true,
	},
	StateFailed:      {StateBackoff: true, StateIdle: true},
	StateBackoff:     {StateChecking: true, StateIdle: true},
	StateSucceeded:   {StateIdle: true, StateChecking: true},
	StateInterrupted: {StateIdle: true},
}

// Event is one structured occurrence for the durable event store (#3
// adapter, wired later).
type Event struct {
	AppID  string
	Time   time.Time
	Kind   string
	From   State
	To     State
	Detail string
}

func (e Event) String() string {
	switch e.Kind {
	case EventState:
		return fmt.Sprintf("%s: %s -> %s: %s", e.AppID, e.From, e.To, e.Detail)
	default:
		return fmt.Sprintf("%s: %s: %s", e.AppID, e.Kind, e.Detail)
	}
}

// EventSink receives scheduler events. It must be fast and must never block
// on network I/O — reporting is not on the critical path; the durable store
// adapter lands later and is local-only.
type EventSink interface {
	RecordAppEvent(ctx context.Context, e Event) error
}
