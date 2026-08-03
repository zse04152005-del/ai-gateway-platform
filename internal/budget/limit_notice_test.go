package budget

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestBudgetLimitNoticeValidationAndSafeJSON(t *testing.T) {
	resetAt := time.Date(2026, time.August, 4, 0, 0, 0, 0, time.UTC)
	valid := []LimitNotice{
		{Level: LimitSoft, RemainingMicros: 10, ResetAt: resetAt},
		{Level: LimitHard, RemainingMicros: 0, ResetAt: resetAt, DegradationHint: DegradeLowerCostModel},
		{Level: LimitHard, RemainingMicros: MaximumAmount, ResetAt: resetAt, DegradationHint: DegradeReduceMaxOutput},
		{Level: LimitSoft, RemainingMicros: 1, ResetAt: resetAt, DegradationHint: DegradeWaitForReset},
	}
	for _, notice := range valid {
		if err := notice.Validate(); err != nil {
			t.Errorf("BudgetLimitNotice.Validate(%+v) error = %v", notice, err)
		}
	}
	invalid := []LimitNotice{
		{},
		{Level: "warning", RemainingMicros: 1, ResetAt: resetAt},
		{Level: LimitHard, RemainingMicros: MaximumAmount + 1, ResetAt: resetAt},
		{Level: LimitSoft, RemainingMicros: 1},
		{Level: LimitHard, RemainingMicros: 1, ResetAt: resetAt, DegradationHint: "switch_tenant"},
	}
	for index, notice := range invalid {
		if err := notice.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("BudgetLimitNotice.Validate(invalid %d) error = %v", index, err)
		}
	}

	account := validAccount()
	failure := newBudgetExceededError(account, DegradeLowerCostModel)
	if failure.Error() != ErrBudgetExceeded.Error() || !errors.Is(failure, ErrBudgetExceeded) ||
		failure.Notice.Level != LimitHard || failure.Notice.RemainingMicros != 850 ||
		!failure.Notice.ResetAt.Equal(account.PeriodEnd) ||
		failure.Notice.DegradationHint != DegradeLowerCostModel || failure.Notice.Validate() != nil {
		t.Fatalf("BudgetExceededError = %+v", failure)
	}
	var typed *HardLimitError
	if !errors.As(failure, &typed) || typed != failure {
		t.Fatalf("errors.As(BudgetExceededError) = %+v", typed)
	}
	encoded, err := json.Marshal(failure)
	if err != nil {
		t.Fatalf("json.Marshal(BudgetExceededError) error = %v", err)
	}
	text := string(encoded)
	for _, secret := range []string{testTenantID, testAccountID, "tenant_id", "account_id", "request_id"} {
		if strings.Contains(text, secret) || strings.Contains(failure.Error(), secret) {
			t.Fatalf("budget error leaked %q: error=%q json=%s", secret, failure.Error(), text)
		}
	}
	for _, required := range []string{`"remaining_micros":850`, `"reset_at":`, `"use_lower_cost_model"`} {
		if !strings.Contains(text, required) {
			t.Fatalf("budget error JSON %s missing %s", text, required)
		}
	}
}

func TestBudgetLimitNoticeRemainingAndHintHelpers(t *testing.T) {
	account := validAccount()
	soft := newBudgetLimitNotice(LimitSoft, account, 900, DegradeReduceMaxOutput)
	if soft.RemainingMicros != 100 || soft.Level != LimitSoft ||
		soft.DegradationHint != DegradeReduceMaxOutput || soft.Validate() != nil {
		t.Fatalf("soft budget notice = %+v", soft)
	}
	account.CommittedMicros = account.HardLimitMicros
	account.ReservedMicros = 0
	hard := newBudgetExceededError(account, "")
	if hard.Notice.RemainingMicros != 0 || hard.Notice.DegradationHint != "" || hard.Notice.Validate() != nil {
		t.Fatalf("exhausted budget notice = %+v", hard.Notice)
	}
	for _, hint := range []DegradationHint{"", DegradeLowerCostModel, DegradeReduceMaxOutput, DegradeWaitForReset} {
		if !validDegradationHint(hint) {
			t.Errorf("validDegradationHint(%q) = false", hint)
		}
	}
	if validDegradationHint("inspect_other_account") {
		t.Fatal("unsafe degradation hint accepted")
	}
}
