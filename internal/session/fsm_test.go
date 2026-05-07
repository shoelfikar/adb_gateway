package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidTransitions(t *testing.T) {
	tests := []struct {
		name string
		from SessionState
		to   SessionState
	}{
		// D-05 transitions
		{"idle to starting", StateIdle, StateStarting},
		{"starting to active", StateStarting, StateActive},
		{"starting to failed", StateStarting, StateFailed},
		{"starting to stopping", StateStarting, StateStopping},
		{"active to stopping", StateActive, StateStopping},
		{"active to failed", StateActive, StateFailed},
		{"stopping to idle", StateStopping, StateIdle},
		{"stopping to failed", StateStopping, StateFailed},
		{"failed to idle (retry)", StateFailed, StateIdle},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			newState, err := TransitionTo(tt.from, tt.to)
			assert.NoError(t, err, "transition %s -> %s should be valid", tt.from, tt.to)
			assert.Equal(t, tt.to, newState)
		})
	}
}

func TestInvalidTransitions(t *testing.T) {
	tests := []struct {
		name string
		from SessionState
		to   SessionState
	}{
		{"idle to active", StateIdle, StateActive},
		{"idle to failed", StateIdle, StateFailed},
		{"idle to stopping", StateIdle, StateStopping},
		{"active to starting", StateActive, StateStarting},
		{"active to idle", StateActive, StateIdle},
		{"starting to idle", StateStarting, StateIdle},
		{"failed to active", StateFailed, StateActive},
		{"failed to starting", StateFailed, StateStarting},
		{"failed to stopping", StateFailed, StateStopping},
		{"stopping to starting", StateStopping, StateStarting},
		{"stopping to active", StateStopping, StateActive},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			newState, err := TransitionTo(tt.from, tt.to)
			assert.Error(t, err, "transition %s -> %s should be invalid", tt.from, tt.to)
			assert.Equal(t, tt.from, newState, "state should not change on invalid transition")
		})
	}
}

func TestCanTransition(t *testing.T) {
	assert.True(t, canTransition(StateIdle, StateStarting))
	assert.True(t, canTransition(StateStarting, StateActive))
	assert.True(t, canTransition(StateFailed, StateIdle))
	assert.False(t, canTransition(StateIdle, StateActive))
	assert.False(t, canTransition(StateActive, StateStarting))
	assert.False(t, canTransition(StateFailed, StateActive))
}

func TestStringRepresentation(t *testing.T) {
	tests := []struct {
		state   SessionState
		want    string
	}{
		{StateIdle, "idle"},
		{StateStarting, "starting"},
		{StateActive, "active"},
		{StateStopping, "stopping"},
		{StateFailed, "failed"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.state.String())
		})
	}

	// Unknown state
	unknownState := SessionState(99)
	assert.Equal(t, "unknown", unknownState.String())
}

func TestTransitionErrorMessage(t *testing.T) {
	_, err := TransitionTo(StateIdle, StateActive)
	assert.Error(t, err)
	// Error message should include from/to state names per D-09.
	assert.Contains(t, err.Error(), "idle")
	assert.Contains(t, err.Error(), "active")
}

func TestFullLifecycleTransition(t *testing.T) {
	// Test the normal lifecycle: idle -> starting -> active -> stopping -> idle
	state := StateIdle

	state, err := TransitionTo(state, StateStarting)
	assert.NoError(t, err)
	assert.Equal(t, StateStarting, state)

	state, err = TransitionTo(state, StateActive)
	assert.NoError(t, err)
	assert.Equal(t, StateActive, state)

	state, err = TransitionTo(state, StateStopping)
	assert.NoError(t, err)
	assert.Equal(t, StateStopping, state)

	state, err = TransitionTo(state, StateIdle)
	assert.NoError(t, err)
	assert.Equal(t, StateIdle, state)
}

func TestRetryCycleTransition(t *testing.T) {
	// Test the failure retry cycle: idle -> starting -> failed -> idle
	state := StateIdle

	state, err := TransitionTo(state, StateStarting)
	assert.NoError(t, err)

	state, err = TransitionTo(state, StateFailed)
	assert.NoError(t, err)
	assert.Equal(t, StateFailed, state)

	// Retry from failed back to idle.
	state, err = TransitionTo(state, StateIdle)
	assert.NoError(t, err)
	assert.Equal(t, StateIdle, state)

	// Can start again after retry.
	state, err = TransitionTo(state, StateStarting)
	assert.NoError(t, err)
	assert.Equal(t, StateStarting, state)
}

func TestStoppingToFailedTransition(t *testing.T) {
	// Test: stopping -> failed (cleanup can fail)
	state := StateStopping

	state, err := TransitionTo(state, StateFailed)
	assert.NoError(t, err)
	assert.Equal(t, StateFailed, state)
}