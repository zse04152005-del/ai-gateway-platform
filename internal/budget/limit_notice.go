package budget

import "time"

// LimitLevel distinguishes an advisory soft crossing from a hard denial.
type LimitLevel string

const (
	// LimitSoft warns that admission succeeded above the soft threshold.
	LimitSoft LimitLevel = "soft"
	// LimitHard explains a hard-limit denial without exposing account identity.
	LimitHard LimitLevel = "hard"
)

// DegradationHint is a finite, non-sensitive action code selected by trusted policy.
type DegradationHint string

const (
	// DegradeLowerCostModel suggests a policy-approved cheaper logical model.
	DegradeLowerCostModel DegradationHint = "use_lower_cost_model"
	// DegradeReduceMaxOutput suggests reducing the requested output allowance.
	DegradeReduceMaxOutput DegradationHint = "reduce_max_output"
	// DegradeWaitForReset suggests retrying after the returned reset boundary.
	DegradeWaitForReset DegradationHint = "wait_for_reset"
)

// LimitNotice is the complete safe client-facing budget metadata. It
// deliberately contains no Tenant, Account, Project, Key or Request identity.
type LimitNotice struct {
	Level           LimitLevel      `json:"level"`
	RemainingMicros uint64          `json:"remaining_micros"`
	ResetAt         time.Time       `json:"reset_at"`
	DegradationHint DegradationHint `json:"degradation_hint,omitempty"`
}

// Validate enforces the stable wire-safe notice shape.
func (notice LimitNotice) Validate() error {
	if (notice.Level != LimitSoft && notice.Level != LimitHard) ||
		notice.RemainingMicros > MaximumAmount || notice.ResetAt.IsZero() ||
		!validDegradationHint(notice.DegradationHint) {
		return ErrInvalid
	}
	return nil
}

// HardLimitError wraps ErrBudgetExceeded with safe structured metadata.
type HardLimitError struct {
	Notice LimitNotice `json:"notice"`
}

func (failure *HardLimitError) Error() string { return ErrBudgetExceeded.Error() }

func (failure *HardLimitError) Unwrap() error { return ErrBudgetExceeded }

func newBudgetLimitNotice(
	level LimitLevel,
	account Account,
	spent uint64,
	hint DegradationHint,
) LimitNotice {
	remaining := uint64(0)
	if spent < account.HardLimitMicros {
		remaining = account.HardLimitMicros - spent
	}
	return LimitNotice{
		Level: level, RemainingMicros: remaining, ResetAt: account.PeriodEnd,
		DegradationHint: hint,
	}
}

func newBudgetExceededError(account Account, hint DegradationHint) *HardLimitError {
	return &HardLimitError{Notice: newBudgetLimitNotice(
		LimitHard, account, account.CommittedMicros+account.ReservedMicros, hint,
	)}
}

func validDegradationHint(hint DegradationHint) bool {
	switch hint {
	case "", DegradeLowerCostModel, DegradeReduceMaxOutput, DegradeWaitForReset:
		return true
	default:
		return false
	}
}
