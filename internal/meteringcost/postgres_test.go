package meteringcost

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestPostgresAggregatorValidatesDependenciesAndInput(t *testing.T) {
	if _, err := NewPostgresAggregator(nil); err == nil {
		t.Fatal("NewPostgresAggregator(nil) error = nil")
	}
	aggregator, err := NewPostgresAggregator(&sql.DB{})
	if err != nil {
		t.Fatalf("NewPostgresAggregator(valid) error = %v", err)
	}
	validScope := Scope{TenantID: testTenantID, ProjectID: testProjectID}
	var nilContext context.Context
	for _, test := range []struct {
		ctx       context.Context
		scope     Scope
		requestID string
	}{
		{ctx: nilContext, scope: validScope, requestID: testRequestID},
		{ctx: context.Background(), scope: Scope{}, requestID: testRequestID},
		{ctx: context.Background(), scope: validScope, requestID: "bad"},
	} {
		if _, aggregateErr := aggregator.Aggregate(test.ctx, test.scope, test.requestID); !errors.Is(aggregateErr, ErrInvalid) {
			t.Errorf("Aggregate(invalid) error = %v", aggregateErr)
		}
	}
	var nilAggregator *PostgresAggregator
	if _, err = nilAggregator.Aggregate(context.Background(), validScope, testRequestID); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil Aggregate() error = %v", err)
	}
}
