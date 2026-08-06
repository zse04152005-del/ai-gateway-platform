package meteringadjustment_test

import (
	"errors"
	"testing"

	"github.com/zse04152005-del/ai-gateway-platform/internal/metering"
	"github.com/zse04152005-del/ai-gateway-platform/internal/meteringadjustment"
)

func TestCommandValidationAndFiniteOrigins(t *testing.T) {
	valid := validCommand()
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid command error = %v", err)
	}
	for _, origin := range []meteringadjustment.Origin{
		meteringadjustment.OriginManual,
		meteringadjustment.OriginProviderReconciliation,
		meteringadjustment.OriginSystemRepair,
	} {
		if !origin.Valid() {
			t.Errorf("origin %q should be valid", origin)
		}
	}
	if meteringadjustment.Origin("provider").Valid() {
		t.Fatal("unknown adjustment origin should be invalid")
	}

	invalid := []meteringadjustment.Command{
		{},
		func() meteringadjustment.Command { value := valid; value.Scope.TenantID = "tenant"; return value }(),
		func() meteringadjustment.Command { value := valid; value.EventID = value.TargetEventID; return value }(),
		func() meteringadjustment.Command { value := valid; value.IdempotencyKey = "short"; return value }(),
		func() meteringadjustment.Command { value := valid; value.Origin = "provider"; return value }(),
		func() meteringadjustment.Command { value := valid; value.Reason = "Free text"; return value }(),
		func() meteringadjustment.Command {
			value := valid
			value.Reference = "ticket with spaces"
			return value
		}(),
		func() meteringadjustment.Command { value := valid; value.Actor = ""; return value }(),
		func() meteringadjustment.Command { value := valid; value.CorrectedQuantity = -1; return value }(),
		func() meteringadjustment.Command {
			value := valid
			value.CorrectedAmountMicros = metering.MaximumExactInteger + 1
			return value
		}(),
	}
	for index, command := range invalid {
		if err := command.Validate(); !errors.Is(err, meteringadjustment.ErrInvalid) {
			t.Errorf("invalid command[%d] error = %v", index, err)
		}
	}
}

func TestPostgresWriterRejectsInvalidDependencies(t *testing.T) {
	if _, err := meteringadjustment.NewPostgresWriter(nil, nil); !errors.Is(err, meteringadjustment.ErrInvalid) {
		t.Fatalf("NewPostgresWriter(nil) error = %v", err)
	}
}

func validCommand() meteringadjustment.Command {
	return meteringadjustment.Command{
		Scope: meteringadjustment.Scope{
			TenantID:  "81000000-0000-4000-8000-000000000001",
			ProjectID: "81000000-0000-4000-8000-000000000002",
		},
		EventID: "81000000-0000-4000-8000-000000000003", IdempotencyKey: "ticket:billing-0001",
		TargetEventID:     "81000000-0000-4000-8000-000000000004",
		CorrectedQuantity: 10, CorrectedAmountMicros: 25,
		Origin: meteringadjustment.OriginManual, Reason: "provider_invoice_correction",
		Reference: "ticket:BILL-0001", Actor: "admin:user-001",
	}
}
