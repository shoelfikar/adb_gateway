package session

import "fmt"

// SessionState represents the lifecycle state of a device session.
// Valid transitions follow D-05 (Phase 1) extended by Plan 03-02 (Phase 3):
//
//	idle    -> starting
//	starting -> active | failed | stopping
//	active  -> stopping | failed | reconnecting   (P3-02: stall detected)
//	reconnecting -> active | failed | stopping    (P3-02: recovery outcome)
//	stopping -> idle | failed
//	failed  -> idle (retry allowed)
type SessionState int

const (
	StateIdle         SessionState = iota // device present, no session
	StateStarting                         // push jar, tunnels, launch
	StateActive                           // streaming
	StateStopping                         // cleanup in progress
	StateFailed                           // terminal state, retry possible
	StateReconnecting                     // P3-02: frame stall detected, recovery in flight
)

// AllStates returns every defined SessionState in iota order. Used by
// observability hooks (e.g. obs.SetSessionState one-hot encoding) so the
// metric stays in sync when new states are added.
func AllStates() []SessionState {
	return []SessionState{
		StateIdle,
		StateStarting,
		StateActive,
		StateStopping,
		StateFailed,
		StateReconnecting,
	}
}

// String returns a human-readable name for the session state.
func (s SessionState) String() string {
	switch s {
	case StateIdle:
		return "idle"
	case StateStarting:
		return "starting"
	case StateActive:
		return "active"
	case StateStopping:
		return "stopping"
	case StateFailed:
		return "failed"
	case StateReconnecting:
		return "reconnecting"
	default:
		return "unknown"
	}
}

// validTransitions defines the allowed state transitions per D-05 + Plan 03-02.
var validTransitions = map[SessionState][]SessionState{
	StateIdle:         {StateStarting},
	StateStarting:     {StateActive, StateFailed, StateStopping},
	StateActive:       {StateStopping, StateFailed, StateReconnecting},
	StateReconnecting: {StateActive, StateFailed, StateStopping},
	StateStopping:     {StateIdle, StateFailed},
	StateFailed:       {StateIdle}, // retry allowed
}

// canTransition checks whether a transition from one state to another is valid.
func canTransition(from, to SessionState) bool {
	for _, valid := range validTransitions[from] {
		if valid == to {
			return true
		}
	}
	return false
}

// TransitionTo validates and performs a state transition.
// Returns the new state on success, or an error with from/to state names
// for slog correlation (D-09).
func TransitionTo(current, target SessionState) (SessionState, error) {
	if !canTransition(current, target) {
		return current, fmt.Errorf("invalid session state transition: %s -> %s", current, target)
	}
	return target, nil
}