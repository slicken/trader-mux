package model

import "testing"

func TestTrailingPhase1GraduateHTF(t *testing.T) {
	cfg := GuardConfig{
		Direction:         "long",
		Tiers:             []GuardTier{{TriggerPct: 0.1, LockPct: 0.03}},
		Phase1Retrace:     0.08,
		Phase1MaxBreaches: 4,
		BreachDecay:       0,
		Phase2Retrace:     0.03,
		Phase2MaxBreaches: 2,
	}
	eng := NewTrailguardStopEngine(cfg)
	st := GuardState{
		EntryPrice:       100,
		Phase1StartTs:    1000,
		CurrentTierIndex: -1,
		HighWater:        100,
		HighWaterTs:      1000,
	}
	res := eng.Evaluate(100.11, st, 10_000)
	if res.Action != GuardTierChanged {
		t.Fatalf("want tier_changed got %s %s", res.Action, res.Reason)
	}
	if res.State.CurrentTierIndex != 0 {
		t.Fatalf("tier index %d", res.State.CurrentTierIndex)
	}
}

func TestTrailingPhase1BreachClose(t *testing.T) {
	cfg := GuardConfig{
		Direction:         "long",
		Tiers:             []GuardTier{{TriggerPct: 99, LockPct: 1}},
		Phase1Retrace:     1.0,
		Phase1MaxBreaches: 2,
		BreachDecay:       0,
		Phase2Retrace:     0.05,
		Phase2MaxBreaches: 2,
	}
	eng := NewTrailguardStopEngine(cfg)
	st := GuardState{
		EntryPrice:       100,
		Phase1StartTs:    1000,
		CurrentTierIndex: -1,
		HighWater:        110,
		HighWaterTs:      1000,
	}
	st.BreachCount = 1
	res := eng.Evaluate(108.8, st, 2000)
	if res.Action != GuardClose {
		t.Fatalf("want close got %s %s", res.Action, res.Reason)
	}
}

func TestGraceBlocksTierPromotion(t *testing.T) {
	cfg := GuardConfig{
		Direction:         "long",
		Tiers:             []GuardTier{{TriggerPct: 0.1, LockPct: 0.03}},
		Phase1Retrace:     0.5,
		Phase1MaxBreaches: 4,
		BreachDecay:       0,
		Phase2Retrace:     0.05,
		Phase2MaxBreaches: 2,
		GracePeriodMs:     5000,
	}
	eng := NewTrailguardStopEngine(cfg)
	st := GuardState{
		EntryPrice:       100,
		Phase1StartTs:    1000,
		CurrentTierIndex: -1,
		HighWater:        100,
		HighWaterTs:      1000,
	}
	res := eng.Evaluate(100.2, st, 2000)
	if res.Action != GuardHold {
		t.Fatalf("grace should block tier, got %s %s", res.Action, res.Reason)
	}
}

func TestBreachDecayFactor(t *testing.T) {
	if decayBreachByFactor(10, 0.85) != 8 {
		t.Fatalf("decay 10*0.85 floor 8")
	}
	if decayBreachByFactor(3, 1.0) != 0 {
		t.Fatal("factor 1 off -> hard reset")
	}
	if decayBreachByFactor(3, 0) != 0 {
		t.Fatal("factor 0 off -> hard reset")
	}
}
