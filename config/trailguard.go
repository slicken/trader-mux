package config

// TrailguardStrategyConfig enables the HTF MM dynamic trail guard on model traders.
// Side (long/short) is always taken from the live position; never configured here.
type TrailguardStrategyConfig struct {
	Enabled bool `json:"enabled"`
	// Mode: "memory" — trail in RAM; exit with reduce-only market when the engine says close.
	// Mode: "book" — maintain a reduce-only stop / stop-limit at the computed floor.
	Mode string `json:"mode"`
	// BookStopTrigger when mode=book: "market" or "limit" → exchange.Order.TriggerType.
	BookStopTrigger string            `json:"book_stop_trigger,omitempty"`
	Guard           TrailguardGuardJSON `json:"guard"`
}

// TrailguardGuardJSON configures Pure Guard without direction or leverage (HTF: tiny real % moves).
type TrailguardGuardJSON struct {
	Tiers []TrailguardTierJSON `json:"tiers,omitempty"`

	// GracePeriodMs: startup window after entry where tier-up, time exits, stagnation, and breach
	// counting are held back so the book can settle. 0 = use default (~2s HTF/MM). -1 = disabled (no grace).
	GracePeriodMs int64 `json:"grace_period_ms,omitempty"`

	Phase1Retrace        float64 `json:"phase1_retrace,omitempty"`
	Phase1AbsoluteFloor  float64 `json:"phase1_absolute_floor,omitempty"`
	Phase1MaxBreaches    int     `json:"phase1_max_breaches,omitempty"`
	Phase1MaxDurationMs  int64   `json:"phase1_max_duration_ms,omitempty"`
	Phase1WeakPeakMs     int64   `json:"phase1_weak_peak_ms,omitempty"`
	Phase1WeakPeakMinRoe float64 `json:"phase1_weak_peak_min_roe,omitempty"`

	Phase2Retrace     float64 `json:"phase2_retrace,omitempty"`
	Phase2MaxBreaches int     `json:"phase2_max_breaches,omitempty"`

	// BreachDecay: on recovery above/below floor, breach count is multiplied by this factor.
	// Values <= 0 or >= 1 are treated as "off" → full reset to 0 (same as old hard decay).
	// Example: 0.85 gently reduces breach pressure each good tick while strictly between 0 and 1.
	BreachDecay float64 `json:"breach_decay,omitempty"`

	StagnationEnabled   bool    `json:"stagnation_enabled,omitempty"`
	StagnationMinRoe    float64 `json:"stagnation_min_roe,omitempty"`
	StagnationTimeoutMs int64   `json:"stagnation_timeout_ms,omitempty"`
}

// TrailguardTierJSON: trigger_pct / lock_pct are real percentage points (0.1 = 0.1% favorable move to arm tier; lock_pct locks that much unrealized % in price vs entry).
type TrailguardTierJSON struct {
	TriggerPct  float64  `json:"trigger_pct"`
	LockPct     float64  `json:"lock_pct"`
	Retrace     *float64 `json:"retrace,omitempty"`
	MaxBreaches *int     `json:"max_breaches,omitempty"`
}
