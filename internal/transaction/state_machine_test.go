package transaction

import (
	"errors"
	"testing"
)

func TestStateTransitions_ValidAndInvalid(t *testing.T) {
	tests := []struct {
		name    string
		from    string
		to      string
		wantErr bool
	}{
		// Valid transitions
		{"created to authenticating", ConnectionStateCreated, ConnectionStateAuthenticating, false},
		{"authenticating to connected", ConnectionStateAuthenticating, ConnectionStateConnected, false},
		{"authenticating to created (cancel)", ConnectionStateAuthenticating, ConnectionStateCreated, false},
		{"connected to validating", ConnectionStateConnected, ConnectionStateValidating, false},
		{"connected to validation_pending", ConnectionStateConnected, ConnectionStateValidationPending, false},
		{"validating to validation_passed", ConnectionStateValidating, ConnectionStateValidationPassed, false},
		{"validating to validation_failed", ConnectionStateValidating, ConnectionStateValidationFailed, false},
		{"validating to requires_reauth", ConnectionStateValidating, ConnectionStateRequiresReauth, false},
		{"validation_failed to validating (retry)", ConnectionStateValidationFailed, ConnectionStateValidating, false},
		{"requires_reauth to authenticating", ConnectionStateRequiresReauth, ConnectionStateAuthenticating, false},
		{"requires_reauth to connected (reconnect)", ConnectionStateRequiresReauth, ConnectionStateConnected, false},
		{"any to disconnected", ConnectionStateConnected, ConnectionStateDisconnected, false},

		// Invalid transitions
		{"created to validating (cannot validate unauthenticated)", ConnectionStateCreated, ConnectionStateValidating, true},
		{"created to validation_passed", ConnectionStateCreated, ConnectionStateValidationPassed, true},
		{"disconnected to validating", ConnectionStateDisconnected, ConnectionStateValidating, true},
		{"disconnected to authenticating (terminal)", ConnectionStateDisconnected, ConnectionStateAuthenticating, true},
		{"disconnected to connected (terminal)", ConnectionStateDisconnected, ConnectionStateConnected, true},
		{"disconnected to sync", ConnectionStateDisconnected, ConnectionStateValidating, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateStateTransition(tt.from, tt.to)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateStateTransition(%q, %q) error = %v, wantErr %v", tt.from, tt.to, err, tt.wantErr)
			}
			if tt.wantErr && err != nil && !errors.Is(err, ErrInvalidStateTransition) {
				t.Errorf("expected ErrInvalidStateTransition, got %v", err)
			}
		})
	}
}

func TestCanSyncConnection(t *testing.T) {
	if err := CanSyncConnection(ConnectionStateDisconnected); !errors.Is(err, ErrConnectionDisconnected) {
		t.Errorf("expected ErrConnectionDisconnected, got %v", err)
	}
	if err := CanSyncConnection(ConnectionStateRequiresReauth); !errors.Is(err, ErrReauthRequired) {
		t.Errorf("expected ErrReauthRequired, got %v", err)
	}
	if err := CanSyncConnection(ConnectionStateAuthenticating); err == nil {
		t.Errorf("expected error for authenticating, got nil")
	}
	if err := CanSyncConnection(ConnectionStateConnected); err != nil {
		t.Errorf("expected nil for connected, got %v", err)
	}
	if err := CanSyncConnection(ConnectionStateValidationPassed); err != nil {
		t.Errorf("expected nil for validation_passed, got %v", err)
	}
	if err := CanSyncConnection(ConnectionStateValidationFailed); err == nil {
		t.Errorf("expected error for validation_failed, got nil")
	}
}

func TestCanValidateConnection(t *testing.T) {
	if err := CanValidateConnection(ConnectionStateDisconnected); !errors.Is(err, ErrConnectionDisconnected) {
		t.Errorf("expected ErrConnectionDisconnected, got %v", err)
	}
	if err := CanValidateConnection(ConnectionStateRequiresReauth); !errors.Is(err, ErrReauthRequired) {
		t.Errorf("expected ErrReauthRequired, got %v", err)
	}
	if err := CanValidateConnection(ConnectionStateCreated); err == nil {
		t.Errorf("expected error for created (unauthenticated), got nil")
	}
	if err := CanValidateConnection(ConnectionStateValidating); err == nil {
		t.Errorf("expected error for validating (already running), got nil")
	}
	if err := CanValidateConnection(ConnectionStateConnected); err != nil {
		t.Errorf("expected nil for connected, got %v", err)
	}
	if err := CanValidateConnection(ConnectionStateValidationFailed); err != nil {
		t.Errorf("expected nil for validation_failed (retry), got %v", err)
	}
}
