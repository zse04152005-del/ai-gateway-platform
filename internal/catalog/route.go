package catalog

import "errors"

// RouteQuery identifies one authorized logical model lookup. Access values
// come from the trusted authentication principal, never from request JSON.
type RouteQuery struct {
	Access       Access
	LogicalModel string
}

// RouteCandidate is one complete, validated logical-to-physical binding.
// It carries secret reference identity but never secret material.
type RouteCandidate struct {
	LogicalModel LogicalModel
	Binding      Binding
	Deployment   Deployment
	Provider     Provider
}

// Validate checks persisted records, active lifecycle, and every relationship.
func (candidate RouteCandidate) Validate() error {
	for _, validation := range []func() error{
		candidate.LogicalModel.Validate,
		candidate.Binding.Validate,
		candidate.Deployment.Validate,
		candidate.Provider.Validate,
	} {
		if err := validation(); err != nil {
			return err
		}
	}
	if candidate.LogicalModel.Status != StatusActive || candidate.Binding.Status != StatusActive ||
		candidate.Deployment.Status != StatusActive || candidate.Provider.Status != StatusActive {
		return errors.New("route candidate records must all be active")
	}
	if candidate.Binding.LogicalModelID != candidate.LogicalModel.ID {
		return errors.New("route candidate binding logical model mismatch")
	}
	if candidate.Binding.DeploymentID != candidate.Deployment.ID {
		return errors.New("route candidate binding deployment mismatch")
	}
	if candidate.Deployment.ProviderID != candidate.Provider.ID {
		return errors.New("route candidate deployment provider mismatch")
	}
	if !candidate.Deployment.Satisfies(candidate.LogicalModel) {
		return errors.New("route candidate deployment violates logical model contract")
	}
	return nil
}

// Clone returns an attempt-safe copy of all pointer and slice fields.
func (candidate RouteCandidate) Clone() RouteCandidate {
	cloned := candidate
	cloned.LogicalModel.RequiredCapabilities.DataRetentionModes = append(
		[]DataRetentionMode(nil),
		candidate.LogicalModel.RequiredCapabilities.DataRetentionModes...,
	)
	if candidate.LogicalModel.Description != nil {
		description := *candidate.LogicalModel.Description
		cloned.LogicalModel.Description = &description
	}
	if candidate.LogicalModel.AllowedRegions != nil {
		regions := append([]string(nil), (*candidate.LogicalModel.AllowedRegions)...)
		cloned.LogicalModel.AllowedRegions = &regions
	}
	if candidate.Deployment.SecretReferenceID != nil {
		secretReferenceID := *candidate.Deployment.SecretReferenceID
		cloned.Deployment.SecretReferenceID = &secretReferenceID
	}
	return cloned
}
