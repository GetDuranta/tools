package devenv

import "testing"

func TestTransitions(t *testing.T) {
	tests := []struct {
		state  State
		action Action
		want   State
	}{
		{StateStopped, ActionStart, StateStarting},
		{StateArchived, ActionStart, StateStarting},
		{StateError, ActionStart, StateStarting},
		{StateRunning, ActionStop, StateStopping},
		{StateRunning, ActionArchive, StateArchiving},
		{StateStopped, ActionArchive, StateArchiving},
		{StateError, ActionArchive, StateArchiving},
		{StateProvisioning, ActionDelete, StateDeleting},
		{StateRunning, ActionDelete, StateDeleting},
		{StateStopped, ActionDelete, StateDeleting},
		{StateArchived, ActionDelete, StateDeleting},
		{StateError, ActionDelete, StateDeleting},
	}
	for _, test := range tests {
		t.Run(string(test.state)+"/"+string(test.action), func(t *testing.T) {
			got, err := Transition(test.state, test.action)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("next = %s, want %s", got, test.want)
			}
		})
	}
}

func TestInvalidTransitions(t *testing.T) {
	tests := []struct {
		state  State
		action Action
	}{
		{StateRunning, ActionStart},
		{StateStopped, ActionStop},
		{StateArchived, ActionStop},
		{StateProvisioning, ActionArchive},
		{StateDeleting, ActionDelete},
		{StateDeleted, ActionDelete},
	}
	for _, test := range tests {
		if _, err := Transition(test.state, test.action); err == nil {
			t.Fatalf("expected %s/%s to fail", test.state, test.action)
		}
	}
}

func TestCompletionStates(t *testing.T) {
	tests := map[State]State{
		StateProvisioning: StateRunning,
		StateStarting:     StateRunning,
		StateStopping:     StateStopped,
		StateArchiving:    StateArchived,
		StateDeleting:     StateDeleted,
	}
	for state, want := range tests {
		got, err := CompletionState(state)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("completion of %s = %s, want %s", state, got, want)
		}
	}
	if _, err := CompletionState(StateRunning); err == nil {
		t.Fatal("RUNNING must not have a workflow completion")
	}
}

func TestWorkspaceSlotStates(t *testing.T) {
	for _, state := range []State{StateProvisioning, StateRunning, StateStopping, StateStopped,
		StateStarting, StateArchiving, StateDeleting, StateError} {
		if !state.HoldsWorkspace() {
			t.Fatalf("%s should hold a workspace slot", state)
		}
	}
	for _, state := range []State{StateArchived, StateDeleted} {
		if state.HoldsWorkspace() {
			t.Fatalf("%s must not hold a workspace slot", state)
		}
	}
}
