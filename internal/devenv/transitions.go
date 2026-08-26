package devenv

import "fmt"

func Transition(state State, action Action) (State, error) {
	var next State
	switch action {
	case ActionStart:
		switch state {
		case StateStopped, StateArchived, StateError:
			next = StateStarting
		}
	case ActionStop:
		if state == StateRunning || state == StateError {
			next = StateStopping
		}
	case ActionArchive:
		switch state {
		case StateRunning, StateStopped, StateError:
			next = StateArchiving
		}
	case ActionDelete:
		if state != StateDeleting && state != StateDeleted {
			next = StateDeleting
		}
	}
	if next == "" {
		return "", fmt.Errorf("cannot %s environment in %s", action, state)
	}
	return next, nil
}

func CompletionState(state State) (State, error) {
	switch state {
	case StateProvisioning, StateStarting:
		return StateRunning, nil
	case StateStopping:
		return StateStopped, nil
	case StateArchiving:
		return StateArchived, nil
	case StateDeleting:
		return StateDeleted, nil
	default:
		return "", fmt.Errorf("state %s has no workflow completion", state)
	}
}

func CanExtend(state State) bool {
	return state == StateRunning || state == StateStarting
}
