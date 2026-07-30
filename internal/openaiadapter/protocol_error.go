package openaiadapter

import "fmt"

// UnsupportedParameterError identifies a normalized field that cannot be mapped safely.
type UnsupportedParameterError struct {
	Field  string
	Reason string
}

// Error implements error without formatting field values.
func (err *UnsupportedParameterError) Error() string {
	if err == nil {
		return "<nil>"
	}
	return fmt.Sprintf("openai adapter field %s is unsupported: %s", err.Field, err.Reason)
}

// Is reports ErrUnsupportedParameter.
func (err *UnsupportedParameterError) Is(target error) bool {
	return err != nil && target == ErrUnsupportedParameter
}

// ProtocolError exposes only a stable parser operation and code.
type ProtocolError struct {
	Operation string
	Code      string
	cause     error
}

// Error never includes provider bodies, prompts, tool arguments, or credentials.
func (err *ProtocolError) Error() string {
	if err == nil {
		return "<nil>"
	}
	return fmt.Sprintf("openai provider %s failed with protocol code %s", err.Operation, err.Code)
}

// Unwrap preserves controlled parser diagnostics.
func (err *ProtocolError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.cause
}

// Is reports protocol and response-size sentinels.
func (err *ProtocolError) Is(target error) bool {
	if err == nil {
		return false
	}
	if target == ErrProtocol {
		return true
	}
	return target == ErrResponseTooLarge && err.cause == ErrResponseTooLarge
}

// ProtocolOperation returns the stable parser stage.
func (err *ProtocolError) ProtocolOperation() string {
	if err == nil {
		return ""
	}
	return err.Operation
}

// ProtocolCode returns the safe violation code.
func (err *ProtocolError) ProtocolCode() string {
	if err == nil {
		return ""
	}
	return err.Code
}

func protocolError(operation, code string, cause error) error {
	return &ProtocolError{Operation: operation, Code: code, cause: cause}
}

func unsupported(field, reason string) error {
	return &UnsupportedParameterError{Field: field, Reason: reason}
}
