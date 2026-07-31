package routing

import (
	"context"
	"errors"
)

// CompositeHealth requires every independent health signal to allow a route.
// Readers run in constructor order so passive and active policy remain
// separately testable and no signal can accidentally override another.
type CompositeHealth struct {
	readers []HealthReader
}

// NewCompositeHealth creates an immutable AND-composition of at least two
// health readers.
func NewCompositeHealth(readers ...HealthReader) (*CompositeHealth, error) {
	if len(readers) < 2 {
		return nil, errors.New("composite health requires at least two readers")
	}
	cloned := make([]HealthReader, len(readers))
	for index, reader := range readers {
		if reader == nil {
			return nil, errors.New("composite health reader must not be nil")
		}
		cloned[index] = reader
	}
	return &CompositeHealth{readers: cloned}, nil
}

// Healthy denies the target on the first unhealthy signal and propagates any
// signal read error so candidate filtering can fail closed.
func (health *CompositeHealth) Healthy(ctx context.Context, deploymentID string) (bool, error) {
	if health == nil || len(health.readers) < 2 {
		return false, errors.New("composite health is not initialized")
	}
	if ctx == nil {
		return false, errors.New("composite health context must not be nil")
	}
	for _, reader := range health.readers {
		healthy, err := reader.Healthy(ctx, deploymentID)
		if err != nil {
			return false, err
		}
		if !healthy {
			return false, nil
		}
	}
	return true, nil
}

var _ HealthReader = (*CompositeHealth)(nil)
