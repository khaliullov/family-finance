package transaction

import (
	"errors"
	"fmt"
)

// Bank connection state constants representing the user-facing lifecycle.
const (
	ConnectionStateCreated           = "created"
	ConnectionStateAuthenticating    = "authenticating"
	ConnectionStateConnected         = "connected"
	ConnectionStateValidationPending = "validation_pending"
	ConnectionStateValidating        = "validating"
	ConnectionStateValidationPassed  = "validation_passed"
	ConnectionStateValidationFailed  = "validation_failed"
	ConnectionStateRequiresReauth    = "requires_reauthentication"
	ConnectionStateError             = "error"
	ConnectionStateDisconnected      = "disconnected"
	ConnectionStateActive            = "active"          // legacy alias
	ConnectionStateExpired           = "expired"         // legacy alias
	ConnectionStatePendingPairing    = "pending_pairing" // legacy alias
)

var (
	ErrInvalidStateTransition = errors.New("invalid bank connection state transition")
	ErrConnectionDisconnected = errors.New("bank connection is disconnected")
	ErrReauthRequired         = errors.New("bank connection requires reauthentication")
	ErrConcurrentModification = errors.New("bank connection state conflict: concurrent update detected")
)

// validTransitions defines permissible transitions between connection states.
var validTransitions = map[string]map[string]bool{
	ConnectionStateCreated: {
		ConnectionStateAuthenticating: true,
		ConnectionStateConnected:      true, // direct manual/fixture seeding
		ConnectionStateDisconnected:   true,
		ConnectionStateError:          true,
	},
	ConnectionStateAuthenticating: {
		ConnectionStateConnected:      true,
		ConnectionStateCreated:        true, // cancelled
		ConnectionStateRequiresReauth: true,
		ConnectionStateError:          true,
		ConnectionStateDisconnected:   true,
	},
	ConnectionStateConnected: {
		ConnectionStateValidationPending: true,
		ConnectionStateValidating:        true,
		ConnectionStateRequiresReauth:    true,
		ConnectionStateError:             true,
		ConnectionStateDisconnected:      true,
		ConnectionStateActive:            true,
	},
	ConnectionStateValidationPending: {
		ConnectionStateValidating:     true,
		ConnectionStateError:          true,
		ConnectionStateDisconnected:   true,
		ConnectionStateRequiresReauth: true,
	},
	ConnectionStateValidating: {
		ConnectionStateValidationPassed: true,
		ConnectionStateValidationFailed: true,
		ConnectionStateRequiresReauth:   true,
		ConnectionStateError:            true,
		ConnectionStateDisconnected:     true,
	},
	ConnectionStateValidationPassed: {
		ConnectionStateValidationPending: true, // re-validation
		ConnectionStateValidating:        true,
		ConnectionStateRequiresReauth:    true,
		ConnectionStateError:             true,
		ConnectionStateDisconnected:      true,
		ConnectionStateActive:            true,
	},
	ConnectionStateValidationFailed: {
		ConnectionStateValidationPending: true, // retry validation
		ConnectionStateValidating:        true,
		ConnectionStateRequiresReauth:    true,
		ConnectionStateError:             true,
		ConnectionStateDisconnected:      true,
	},
	ConnectionStateRequiresReauth: {
		ConnectionStateAuthenticating: true,
		ConnectionStateConnected:      true,
		ConnectionStateDisconnected:   true,
	},
	ConnectionStateError: {
		ConnectionStateAuthenticating:    true,
		ConnectionStateConnected:         true,
		ConnectionStateValidationPending: true,
		ConnectionStateDisconnected:      true,
	},
	ConnectionStateActive: {
		ConnectionStateAuthenticating:    true,
		ConnectionStateConnected:         true,
		ConnectionStateValidationPending: true,
		ConnectionStateValidating:        true,
		ConnectionStateRequiresReauth:    true,
		ConnectionStateError:             true,
		ConnectionStateDisconnected:      true,
	},
	ConnectionStateExpired: {
		ConnectionStateAuthenticating: true,
		ConnectionStateConnected:      true,
		ConnectionStateDisconnected:   true,
	},
	ConnectionStatePendingPairing: {
		ConnectionStateConnected:    true,
		ConnectionStateActive:       true,
		ConnectionStateDisconnected: true,
		ConnectionStateError:        true,
	},
	ConnectionStateDisconnected: {
		// Terminal state: cannot transition out. Reconnect does not resurrect disconnected connections.
		// A user must create a new connection.
	},
}

// ValidateStateTransition checks if a transition from current to target is allowed.
func ValidateStateTransition(current, target string) error {
	if current == target {
		return nil
	}
	allowed, ok := validTransitions[current]
	if !ok || !allowed[target] {
		return fmt.Errorf("%w: cannot transition from %q to %q", ErrInvalidStateTransition, current, target)
	}
	return nil
}

// CanSyncConnection checks if a connection status allows financial transaction synchronization.
func CanSyncConnection(status string) error {
	switch status {
	case ConnectionStateDisconnected:
		return ErrConnectionDisconnected
	case ConnectionStateRequiresReauth, ConnectionStateExpired:
		return ErrReauthRequired
	case ConnectionStateCreated, ConnectionStateAuthenticating, ConnectionStateValidating, ConnectionStateValidationPending:
		return fmt.Errorf("sync rejected: connection is in %q state", status)
	case ConnectionStateValidationFailed:
		return fmt.Errorf("sync rejected: connection validation failed")
	case ConnectionStateActive, ConnectionStateConnected, ConnectionStateValidationPassed, ConnectionStateError, ConnectionStatePendingPairing:
		return nil
	default:
		return fmt.Errorf("sync rejected: connection status %q does not allow synchronization", status)
	}
}

// CanValidateConnection checks if a connection status allows initiating validation.
func CanValidateConnection(status string) error {
	switch status {
	case ConnectionStateDisconnected:
		return ErrConnectionDisconnected
	case ConnectionStateRequiresReauth, ConnectionStateExpired:
		return ErrReauthRequired
	case ConnectionStateCreated:
		return fmt.Errorf("validation rejected: connection is not authenticated (status is %q)", status)
	case ConnectionStateAuthenticating:
		return fmt.Errorf("validation rejected: connection is currently authenticating")
	case ConnectionStateValidating:
		return fmt.Errorf("validation rejected: validation is already running")
	case ConnectionStateConnected, ConnectionStateValidationPassed, ConnectionStateValidationFailed, ConnectionStateActive, ConnectionStateValidationPending, ConnectionStateError:
		return nil
	default:
		return fmt.Errorf("validation rejected: connection status %q does not allow validation", status)
	}
}
