package catalog

import "errors"

// HealthProbeTarget is one active, chat-capable physical deployment that may
// receive an internal low-cost health request. It contains a secret reference
// identifier, never secret material.
type HealthProbeTarget struct {
	Provider   Provider
	Deployment Deployment
}

// Validate checks lifecycle, relationship, and probe protocol support.
func (target HealthProbeTarget) Validate() error {
	if err := target.Provider.Validate(); err != nil {
		return err
	}
	if err := target.Deployment.Validate(); err != nil {
		return err
	}
	if target.Provider.Status != StatusActive || target.Deployment.Status != StatusActive {
		return errors.New("health probe target must be active")
	}
	if target.Deployment.ProviderID != target.Provider.ID {
		return errors.New("health probe deployment provider mismatch")
	}
	if !target.Deployment.Capabilities.Chat {
		return errors.New("health probe target must support chat")
	}
	return nil
}

// Clone returns a defensive copy of pointer-bearing target fields.
func (target HealthProbeTarget) Clone() HealthProbeTarget {
	cloned := target
	if target.Deployment.SecretReferenceID != nil {
		secretReferenceID := *target.Deployment.SecretReferenceID
		cloned.Deployment.SecretReferenceID = &secretReferenceID
	}
	return cloned
}
