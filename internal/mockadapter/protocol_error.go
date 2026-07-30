package mockadapter

import "fmt"

// UnsupportedParameterError identifies a safe normalized field that the Mock
// protocol cannot express without silently changing semantics.
type UnsupportedParameterError struct {
	Field  string
	Reason string
}

// Error implements error without formatting field values.
func (err *UnsupportedParameterError) Error() string {
	if err == nil {
		return "<nil>"
	}
	return fmt.Sprintf("mock adapter field %s is unsupported: %s", err.Field, err.Reason)
}

// Is reports ErrUnsupportedParameter.
func (err *UnsupportedParameterError) Is(target error) bool {
	return err != nil && target == ErrUnsupportedParameter
}

// ProtocolError contains a stable operation/code while keeping parser details private.
type ProtocolError struct {
	Operation string
	Code      string
	cause     error
}

// Error never includes response bodies, prompts, tool arguments, or cause text.
func (err *ProtocolError) Error() string {
	if err == nil {
		return "<nil>"
	}
	return fmt.Sprintf("mock provider %s failed with protocol code %s", err.Operation, err.Code)
}

// Unwrap preserves programmatic parser diagnostics.
func (err *ProtocolError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.cause
}

// Is reports ErrProtocol and the size-specific sentinel when applicable.
func (err *ProtocolError) Is(target error) bool {
	if err == nil {
		return false
	}
	if target == ErrProtocol {
		return true
	}
	return target == ErrResponseTooLarge && err.cause == ErrResponseTooLarge
}

func protocolError(operation, code string, cause error) error {
	return &ProtocolError{Operation: operation, Code: code, cause: cause}
}

func unsupported(field, reason string) error {
	return &UnsupportedParameterError{Field: field, Reason: reason}
}
