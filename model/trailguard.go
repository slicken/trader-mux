/*
HTF market-maker trailing (Pure Guard) — trailguard.go
[orginal code @https://github.com/Nunchi-trade/agent-cli/blob/main/modules/trailing_stop.py]

This file is self-contained: the deterministic “guard” engine plus wiring to exchange.I
for reduce-only exits and optional resting stops.

What it does (high level)
-------------------------
1) Every model tick, Marketmaker.manageOrders() looks at exchange positions. For each
   pair with non-zero size it calls trader.manageOrders() for that pair’s trader only.
   Flat pairs still get one cheap pass to cancel resting trail stops left from book mode.

2) Side is never configured: long positions (size > 0) always trail as long; shorts as short.
   Entry is position average price; mark is bid/ask mid when available, else pair mid/mark.

3) Two exchange behaviours (config model.trailguard.mode):
   - "memory" (default): no orders are placed for the trail. When the engine returns a
     close action, the bot sends a single reduce-only MARKET exit for the full size.
   - "book": after the grace window, the bot keeps a reduce-only stop (or stop-limit) at
     the engine’s effective price floor, cancelling/replacing when the floor moves enough
     (tick-sized deadband). During grace, no book stop is placed so the book can settle.

4) Phases
   - Phase 1 (current_tier_index < 0): “breathe” — wide retrace from high-water, breach
     counting before exit. Optional time-based weak-peak cut and max-duration exit.
   - Phase 2 (tier >= 0): tighter retrace, tier ratchets by favorable ROE, optional
     stagnation take-profit, breach-based exit.

Percent semantics (critical)
----------------------------
All *_pct / retrace / ROE fields use REAL percentage points as elsewhere in this repo:
  0.1 means 0.1%, 1.0 means 1%, not decimals of 1.
ROE is unlevered: favorable move from entry as (delta/entry)*100.
Retrace from high-water: long trailing floor = high_water * (1 - phase_retrace/100), etc.
Tier lock floor: long = entry * (1 + lock_pct/100), short = entry * (1 - lock_pct/100).

JSON settings (config.Model.Trailguard)
-------------------------------------
TrailguardStrategyConfig (model.trailguard in JSON):
  enabled          bool    — master switch; if false, manageOrders returns immediately.
  mode             string  — "memory" | "book" (case-insensitive; default memory-like if not "book").
  book_stop_trigger string — when mode=book: "market" | "limit" → Order.TriggerType for stop wiring.
  guard            object  — TrailguardGuardJSON (see below).

TrailguardGuardJSON (model.trailguard.guard):
  tiers[] (optional)
      trigger_pct   — minimum favorable ROE% (unlevered) to activate this tier (phase 2)
                      or to graduate from phase 1 into tier 0 (first tier’s trigger).
      lock_pct      — once tier is active, minimum locked profit vs entry in % (price floor).
      retrace       — optional override; if set, phase-2 retrace from high-water uses this % instead of phase2_retrace.
      max_breaches  — optional override for phase-2 max breaches for this tier.

  If tiers is empty, defaults to a single tier: trigger_pct=0.08, lock_pct=0.025 (HTF/MM micro).

  grace_period_ms int64
      After each new position session starts, for this many milliseconds:
        - no tier promotion, no phase1 time exits, no stagnation, no breach increments;
        - high-water still updates; book stops are not placed/refreshed.
      -1 = disable grace entirely (0 ms in engine).
       0 or omit = use built-in default 2000 ms (HTF/MM post-fill).

  phase1_retrace float64 — % pullback from high-water (long) before counting a breach; default 0.1 if 0.
  phase1_absolute_floor float64 — optional absolute price floor/ceiling; 0 = disabled (see engine).
  phase1_max_breaches int — breaches before phase1 exit; default 3 if 0.

  breach_decay float64 — on recovery above/below the effective floor (not during grace):
      <= 0 or >= 1  → “off”: breach count resets to 0.
      strictly (0,1) → count becomes floor(count * factor) each recovery tick.

  phase1_max_duration_ms / phase1_weak_peak_ms / phase1_weak_peak_min_roe
      Optional time-based exits in phase 1 (0 disables each).

  phase2_retrace float64 — % retrace from high-water in phase 2; default 0.04 if 0.
  phase2_max_breaches int — default 2 if 0.

  stagnation_enabled / stagnation_min_roe / stagnation_timeout_ms
      Optional: if ROE >= min and high-water has not improved for timeout_ms, market exit.

Engine outputs (GuardAction)
-----------------------------
  hold, close, tier_changed, phase1_timeout, weak_peak_cut — drive logging and exchange actions.

State (TrailguardData on each trader)
------------------------------------
SessionKey ties state to (avg entry, side). BookStopOrderID / LastSyncedFloor support book mode.
*/

package model

import (
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
	"time"

	"ticktrader/config"
	"ticktrader/exchange"
)

// --- Guard engine (pure) ----------------------------------------------------

// GuardAction is the outcome of one trailing evaluation tick.
type GuardAction string

const (
	GuardHold          GuardAction = "hold"
	GuardClose         GuardAction = "close"
	GuardTierChanged   GuardAction = "tier_changed"
	GuardPhase1Timeout GuardAction = "phase1_timeout"
	GuardWeakPeakCut   GuardAction = "weak_peak_cut"
)

// GuardTier is one profit-lock tier (phase 2 ratchet). Pct are real % (0.1 = 0.1%).
type GuardTier struct {
	TriggerPct  float64
	LockPct     float64
	Retrace     *float64 // nil → use global phase2 retrace
	MaxBreaches *int     // nil → use global phase2 max breaches
}

// GuardConfig is static Pure Guard configuration (no I/O). Direction is always from the open position.
type GuardConfig struct {
	Direction string // "long" or "short" — set from Position.Size each tick

	Tiers []GuardTier

	// GracePeriodMs: after Phase1StartTs, hold back tier-up, timers, stagnation, and breach counting.
	// 0 in engine means feature unused (caller resolves default vs disabled).
	GracePeriodMs int64

	Phase1Retrace       float64
	Phase1AbsoluteFloor float64
	Phase1MaxBreaches   int

	// BreachDecay: (0,1) multiply breach count on recovery; <=0 or >=1 → reset count to 0 on recovery.
	BreachDecay float64

	Phase1MaxDurationMs  int64
	Phase1WeakPeakMs     int64
	Phase1WeakPeakMinRoe float64

	Phase2Retrace     float64
	Phase2MaxBreaches int

	StagnationEnabled   bool
	StagnationMinRoe    float64
	StagnationTimeoutMs int64
}

// GuardState is mutable trailing state for one open position session.
type GuardState struct {
	HighWater        float64
	HighWaterTs      int64
	CurrentRoe       float64
	CurrentTierIndex int // <0 = phase 1
	BreachCount      int
	EntryPrice       float64
	Phase1StartTs    int64
}

// GuardResult is the output of TrailguardStopEngine.Evaluate.
type GuardResult struct {
	Action         GuardAction
	State          GuardState
	Reason         string
	NewTierIndex   *int
	EffectiveFloor float64
	TrailingFloor  float64
	TierFloor      float64
	RoePct         float64
}

// TrailguardStopEngine evaluates one price tick against GuardConfig (pure, deterministic).
type TrailguardStopEngine struct {
	cfg GuardConfig
}

// NewTrailguardStopEngine builds an engine with a copy of cfg.
func NewTrailguardStopEngine(cfg GuardConfig) *TrailguardStopEngine {
	c := cfg
	return &TrailguardStopEngine{cfg: c}
}

func (e *TrailguardStopEngine) inGrace(s GuardState, nowMs int64) bool {
	ms := e.cfg.GracePeriodMs
	if ms <= 0 {
		return false
	}
	return s.Phase1StartTs > 0 && nowMs-s.Phase1StartTs < ms
}

// Evaluate applies one tick: updates high water, ROE, phase logic; returns action + new state.
func (e *TrailguardStopEngine) Evaluate(price float64, state GuardState, nowMs int64) GuardResult {
	cfg := e.cfg
	s := state
	isLong := strings.EqualFold(cfg.Direction, "long")
	grace := e.inGrace(s, nowMs)

	if isLong {
		if price > s.HighWater {
			s.HighWater = price
			s.HighWaterTs = nowMs
		}
	} else {
		if s.HighWater == 0 || price < s.HighWater {
			s.HighWater = price
			s.HighWaterTs = nowMs
		}
	}

	roe := computeRoe(price, &s, &cfg)
	s.CurrentRoe = roe

	if s.CurrentTierIndex < 0 {
		return e.phase1(price, s, roe, nowMs, grace)
	}
	return e.phase2(price, s, roe, nowMs, grace)
}

// computeRoe is unlevered favorable return in real percent (0.1 = 0.1% on entry).
func computeRoe(price float64, state *GuardState, cfg *GuardConfig) float64 {
	entry := state.EntryPrice
	if entry <= 0 {
		return 0
	}
	if strings.EqualFold(cfg.Direction, "long") {
		return (price - entry) / entry * 100
	}
	return (entry - price) / entry * 100
}

func (e *TrailguardStopEngine) phase1(price float64, s GuardState, roe float64, nowMs int64, grace bool) GuardResult {
	cfg := e.cfg
	isLong := strings.EqualFold(cfg.Direction, "long")

	if !grace && s.Phase1StartTs > 0 {
		elapsed := nowMs - s.Phase1StartTs

		if cfg.Phase1MaxDurationMs > 0 && elapsed >= cfg.Phase1MaxDurationMs {
			return GuardResult{
				Action: GuardPhase1Timeout,
				State:  s,
				Reason: fmt.Sprintf(
					"Phase 1 timeout: %.0fmin >= %.0fmin limit, ROE=%.3f%%",
					float64(elapsed)/60000, float64(cfg.Phase1MaxDurationMs)/60000, roe,
				),
				RoePct: roe,
			}
		}

		if cfg.Phase1WeakPeakMs > 0 && elapsed >= cfg.Phase1WeakPeakMs && s.HighWater > 0 {
			peakRoe := computeRoe(s.HighWater, &s, &cfg)
			if peakRoe < cfg.Phase1WeakPeakMinRoe {
				return GuardResult{
					Action: GuardWeakPeakCut,
					State:  s,
					Reason: fmt.Sprintf(
						"Weak peak cut: %.0fmin elapsed, peak ROE=%.3f%% < %.3f%%",
						float64(elapsed)/60000, peakRoe, cfg.Phase1WeakPeakMinRoe,
					),
					RoePct: roe,
				}
			}
		}
	}

	if !grace && len(cfg.Tiers) > 0 && roe >= cfg.Tiers[0].TriggerPct {
		s.CurrentTierIndex = 0
		s.BreachCount = 0
		tierFl := tierFloorPrice(0, &s, &cfg)
		ti := 0
		return GuardResult{
			Action:         GuardTierChanged,
			State:          s,
			Reason:         fmt.Sprintf("Phase 1->2: tier 0 (ROE %.4f%% >= %.4f%%)", roe, cfg.Tiers[0].TriggerPct),
			NewTierIndex:   &ti,
			EffectiveFloor: tierFl,
			TierFloor:      tierFl,
			TrailingFloor:  0,
			RoePct:         roe,
		}
	}

	retrace := cfg.Phase1Retrace
	var trailingFl, effectiveFl float64
	var isBreach bool
	if isLong {
		trailingFl = s.HighWater * (1.0 - retrace/100.0)
		absFl := cfg.Phase1AbsoluteFloor
		if absFl > 0 {
			effectiveFl = max(trailingFl, absFl)
		} else {
			effectiveFl = trailingFl
		}
		isBreach = price <= effectiveFl
	} else {
		trailingFl = s.HighWater * (1.0 + retrace/100.0)
		absFl := cfg.Phase1AbsoluteFloor
		if absFl > 0 {
			effectiveFl = min(trailingFl, absFl)
		} else {
			effectiveFl = trailingFl
		}
		isBreach = price >= effectiveFl
	}

	if isBreach && !grace {
		s.BreachCount++
		if s.BreachCount >= cfg.Phase1MaxBreaches {
			return GuardResult{
				Action:         GuardClose,
				State:          s,
				Reason:         fmt.Sprintf("Phase 1 close: %d/%d breaches, price=%.6f, floor=%.6f", s.BreachCount, cfg.Phase1MaxBreaches, price, effectiveFl),
				EffectiveFloor: effectiveFl,
				TrailingFloor:  trailingFl,
				RoePct:         roe,
			}
		}
	} else if !isBreach && !grace {
		s.BreachCount = decayBreachByFactor(s.BreachCount, cfg.BreachDecay)
	}

	if grace {
		return GuardResult{
			Action:         GuardHold,
			State:          s,
			Reason:         fmt.Sprintf("grace: ROE=%.4f%%, HW=%.6f", roe, s.HighWater),
			EffectiveFloor: effectiveFl,
			TrailingFloor:  trailingFl,
			RoePct:         roe,
		}
	}

	return GuardResult{
		Action:         GuardHold,
		State:          s,
		Reason:         fmt.Sprintf("Phase 1: ROE=%.4f%%, HW=%.6f, breaches=%d", roe, s.HighWater, s.BreachCount),
		EffectiveFloor: effectiveFl,
		TrailingFloor:  trailingFl,
		RoePct:         roe,
	}
}

func (e *TrailguardStopEngine) phase2(price float64, s GuardState, roe float64, nowMs int64, grace bool) GuardResult {
	cfg := e.cfg
	isLong := strings.EqualFold(cfg.Direction, "long")

	tierChanged := false
	prevTier := s.CurrentTierIndex
	if !grace {
		for s.CurrentTierIndex+1 < len(cfg.Tiers) && roe >= cfg.Tiers[s.CurrentTierIndex+1].TriggerPct {
			s.CurrentTierIndex++
			s.BreachCount = 0
			tierChanged = true
		}
	}

	tier := cfg.Tiers[s.CurrentTierIndex]

	if !grace && cfg.StagnationEnabled && roe >= cfg.StagnationMinRoe {
		staleMs := nowMs - s.HighWaterTs
		if staleMs >= cfg.StagnationTimeoutMs {
			tierFl := tierFloorPrice(s.CurrentTierIndex, &s, &cfg)
			return GuardResult{
				Action:         GuardClose,
				State:          s,
				Reason:         fmt.Sprintf("Stagnation TP: ROE=%.3f%% >= %.3f%%, HW stale %.0fs", roe, cfg.StagnationMinRoe, float64(staleMs)/1000),
				EffectiveFloor: tierFl,
				TierFloor:      tierFl,
				RoePct:         roe,
			}
		}
	}

	tierFl := tierFloorPrice(s.CurrentTierIndex, &s, &cfg)
	retrace := cfg.Phase2Retrace
	if tier.Retrace != nil {
		retrace = *tier.Retrace
	}

	var trailingFl, effectiveFl float64
	var isBreach bool
	if isLong {
		trailingFl = s.HighWater * (1.0 - retrace/100.0)
		effectiveFl = max(tierFl, trailingFl)
		isBreach = price <= effectiveFl
	} else {
		trailingFl = s.HighWater * (1.0 + retrace/100.0)
		effectiveFl = min(tierFl, trailingFl)
		isBreach = price >= effectiveFl
	}

	if tierChanged {
		return GuardResult{
			Action:         GuardTierChanged,
			State:          s,
			Reason:         fmt.Sprintf("Tier upgrade: %d->%d (ROE %.4f%% >= %.4f%%)", prevTier, s.CurrentTierIndex, roe, tier.TriggerPct),
			NewTierIndex:   &s.CurrentTierIndex,
			EffectiveFloor: effectiveFl,
			TrailingFloor:  trailingFl,
			TierFloor:      tierFl,
			RoePct:         roe,
		}
	}

	maxBreaches := cfg.Phase2MaxBreaches
	if tier.MaxBreaches != nil {
		maxBreaches = *tier.MaxBreaches
	}

	if isBreach && !grace {
		s.BreachCount++
		if s.BreachCount >= maxBreaches {
			idx := s.CurrentTierIndex
			return GuardResult{
				Action:         GuardClose,
				State:          s,
				Reason:         fmt.Sprintf("Phase 2 close: tier %d, %d/%d breaches, price=%.6f, floor=%.6f", s.CurrentTierIndex, s.BreachCount, maxBreaches, price, effectiveFl),
				NewTierIndex:   &idx,
				EffectiveFloor: effectiveFl,
				TrailingFloor:  trailingFl,
				TierFloor:      tierFl,
				RoePct:         roe,
			}
		}
	} else if !isBreach && !grace {
		s.BreachCount = decayBreachByFactor(s.BreachCount, cfg.BreachDecay)
	}

	if grace {
		return GuardResult{
			Action:         GuardHold,
			State:          s,
			Reason:         fmt.Sprintf("grace phase2: tier %d ROE=%.4f%%", s.CurrentTierIndex, roe),
			EffectiveFloor: effectiveFl,
			TrailingFloor:  trailingFl,
			TierFloor:      tierFl,
			RoePct:         roe,
		}
	}

	return GuardResult{
		Action:         GuardHold,
		State:          s,
		Reason:         fmt.Sprintf("Phase 2: tier %d, ROE=%.4f%%, HW=%.6f, breaches=%d", s.CurrentTierIndex, roe, s.HighWater, s.BreachCount),
		EffectiveFloor: effectiveFl,
		TrailingFloor:  trailingFl,
		TierFloor:      tierFl,
		RoePct:         roe,
	}
}

// tierFloorPrice: lock_pct is real % move locked vs entry (unlevered), e.g. 0.03 → 0.03% from entry toward profit.
func tierFloorPrice(tierIndex int, state *GuardState, cfg *GuardConfig) float64 {
	if tierIndex < 0 || tierIndex >= len(cfg.Tiers) {
		return 0
	}
	tier := cfg.Tiers[tierIndex]
	entry := state.EntryPrice
	if entry <= 0 {
		return 0
	}
	lock := tier.LockPct / 100.0
	if strings.EqualFold(cfg.Direction, "long") {
		return entry * (1.0 + lock)
	}
	return entry * (1.0 - lock)
}

func decayBreachByFactor(count int, factor float64) int {
	if count <= 0 {
		return 0
	}
	if factor <= 0 || factor >= 1 {
		return 0
	}
	return int(math.Floor(float64(count) * factor))
}

// --- TrailguardData + config merge + exchange wiring -------------------------

// TrailguardData stores Pure Guard session state and the last resting exit stop id (book mode).
type TrailguardData struct {
	SessionKey      string
	State           GuardState
	BookStopOrderID string
	LastSyncedFloor float64
}

func trailguardModeBook(tc *config.TrailguardStrategyConfig) bool {
	return tc != nil && strings.EqualFold(strings.TrimSpace(tc.Mode), "book")
}

func trailguardBookTriggerType(tc *config.TrailguardStrategyConfig) string {
	if tc == nil {
		return "market"
	}
	switch strings.ToLower(strings.TrimSpace(tc.BookStopTrigger)) {
	case "limit":
		return "limit"
	default:
		return "market"
	}
}

// resolvedGracePeriodMS: -1 in config → disabled (0). 0 / omitted → default 2s HTF/MM post-fill settle.
func resolvedGracePeriodMS(g *config.TrailguardGuardJSON) int64 {
	if g == nil {
		return 2000
	}
	if g.GracePeriodMs == -1 {
		return 0
	}
	if g.GracePeriodMs <= 0 {
		return 2000
	}
	return g.GracePeriodMs
}

func graceActive(phase1StartTs, nowMs, graceMs int64) bool {
	if graceMs <= 0 || phase1StartTs <= 0 {
		return false
	}
	return nowMs-phase1StartTs < graceMs
}

func guardConfigFromJSON(g *config.TrailguardGuardJSON, pos exchange.Position) GuardConfig {
	src := g
	if src == nil {
		src = &config.TrailguardGuardJSON{}
	}
	tiers := make([]GuardTier, 0, len(src.Tiers)+1)
	for _, tier := range src.Tiers {
		tiers = append(tiers, GuardTier{
			TriggerPct:  tier.TriggerPct,
			LockPct:     tier.LockPct,
			Retrace:     tier.Retrace,
			MaxBreaches: tier.MaxBreaches,
		})
	}
	if len(tiers) == 0 {
		tiers = []GuardTier{
			{TriggerPct: 0.08, LockPct: 0.025},
		}
	}

	p1r := src.Phase1Retrace
	if p1r == 0 {
		p1r = 0.1
	}
	p2r := src.Phase2Retrace
	if p2r == 0 {
		p2r = 0.04
	}
	p1b := src.Phase1MaxBreaches
	if p1b == 0 {
		p1b = 3
	}
	p2b := src.Phase2MaxBreaches
	if p2b == 0 {
		p2b = 2
	}

	dir := "long"
	if pos.Size < 0 {
		dir = "short"
	}

	return GuardConfig{
		Direction:            dir,
		Tiers:                tiers,
		GracePeriodMs:        resolvedGracePeriodMS(src),
		Phase1Retrace:        p1r,
		Phase1AbsoluteFloor:  src.Phase1AbsoluteFloor,
		Phase1MaxBreaches:    p1b,
		BreachDecay:          src.BreachDecay,
		Phase1MaxDurationMs:  src.Phase1MaxDurationMs,
		Phase1WeakPeakMs:     src.Phase1WeakPeakMs,
		Phase1WeakPeakMinRoe: src.Phase1WeakPeakMinRoe,
		Phase2Retrace:        p2r,
		Phase2MaxBreaches:    p2b,
		StagnationEnabled:    src.StagnationEnabled,
		StagnationMinRoe:     src.StagnationMinRoe,
		StagnationTimeoutMs:  src.StagnationTimeoutMs,
	}
}

func trailguardSessionKey(pos exchange.Position) string {
	sign := 0
	switch {
	case pos.Size > 0:
		sign = 1
	case pos.Size < 0:
		sign = -1
	}
	return strconv.FormatFloat(pos.AvgPrice, 'g', 12, 64) + "|" + strconv.Itoa(sign)
}

func trailguardMarkPrice(t *trader) float64 {
	if t.bestBid > 0 && t.bestAsk > 0 {
		return (t.bestBid + t.bestAsk) / 2
	}
	if t.MidPrice > 0 {
		return t.MidPrice
	}
	return t.MarkPrice
}

func roundToTick(p, tick float64) float64 {
	if tick <= 0 || p <= 0 {
		return p
	}
	return math.Round(p/tick) * tick
}

func closeSideForPosition(pos exchange.Position) string {
	if pos.Size > 0 {
		return "sell"
	}
	return "buy"
}

// manageOrders is the HTF trailing / exit-stop manager: long positions trail long, short trail short.
// With trailing disabled or no config, returns immediately. Flat pairs only run book cleanup when needed.
func (t *trader) manageOrders() {
	if t == nil || t.parent == nil || t.parent.Exchange == nil {
		return
	}
	cfg := t.parent.config
	if cfg == nil || cfg.Trailguard == nil || !cfg.Trailguard.Enabled {
		return
	}
	tc := cfg.Trailguard

	pos, err := t.parent.Exchange.GetPosition(t.Pair)
	if err != nil || pos.Size == 0 {
		if trailguardModeBook(tc) && (t.Trailguard.SessionKey != "" || t.Trailguard.LastSyncedFloor > 0 || t.Trailguard.BookStopOrderID != "") {
			t.cancelManagedExitStops(nil)
		}
		t.Lock()
		t.Trailguard = TrailguardData{}
		t.Unlock()
		return
	}

	t.RLock()
	price := trailguardMarkPrice(t)
	t.RUnlock()
	if price <= 0 {
		return
	}

	guardCfg := guardConfigFromJSON(&tc.Guard, pos)
	nowMs := time.Now().UnixMilli()

	t.Lock()
	defer t.Unlock()

	sk := trailguardSessionKey(pos)
	if t.Trailguard.SessionKey != sk {
		t.Trailguard = TrailguardData{
			SessionKey: sk,
			State: GuardState{
				EntryPrice:       pos.AvgPrice,
				Phase1StartTs:    nowMs,
				CurrentTierIndex: -1,
				BreachCount:      0,
				HighWater:        0,
				HighWaterTs:      nowMs,
			},
		}
	}

	eng := NewTrailguardStopEngine(guardCfg)
	res := eng.Evaluate(price, t.Trailguard.State, nowMs)
	t.Trailguard.State = res.State

	switch res.Action {
	case GuardClose, GuardPhase1Timeout, GuardWeakPeakCut:
		if trailguardModeBook(tc) {
			t.cancelManagedExitStops(&pos)
		}
		t.placeReduceOnlyMarketExit(pos, res.Reason)
		t.Trailguard = TrailguardData{}
		return
	default:
		if trailguardModeBook(tc) &&
			!graceActive(t.Trailguard.State.Phase1StartTs, nowMs, guardCfg.GracePeriodMs) {
			t.syncTrailguardBookStop(tc, pos, res)
		}
	}
}

func (t *trader) placeReduceOnlyMarketExit(pos exchange.Position, reason string) {
	side := closeSideForPosition(pos)
	o := exchange.Order{
		Pair:       t.Pair,
		Side:       side,
		Type:       "market",
		Size:       math.Abs(pos.Size),
		Price:      0,
		ReduceOnly: true,
	}
	if err := t.parent.Exchange.PlaceOrders([]exchange.Order{o}); err != nil {
		log.Printf("trailing exit %s: %v (%s)", t.Pair, err, reason)
		return
	}
	log.Printf("trailing exit %s %s size=%.6f — %s", t.Pair, side, o.Size, reason)
}

// cancelManagedExitStops removes reduce-only stop/stop-limit orders (trigger + not take-profit).
// If pos is nil (flat), cancels matching exit stops for both sides on the pair.
func (t *trader) cancelManagedExitStops(pos *exchange.Position) {
	orders := t.parent.Exchange.GetOrders()
	var batch []exchange.Order
	for _, o := range orders {
		if o.Pair != t.Pair || !o.ReduceOnly || o.TriggerPrice <= 0 || o.TakeProfit {
			continue
		}
		if pos != nil {
			want := closeSideForPosition(*pos)
			if o.Side != want {
				continue
			}
		}
		if o.Status != "open" {
			continue
		}
		batch = append(batch, o)
	}
	if len(batch) == 0 {
		return
	}
	if err := t.parent.Exchange.CancelOrders(batch); err != nil {
		log.Printf("trailing cancel stops %s: %v", t.Pair, err)
	}
	t.Trailguard.BookStopOrderID = ""
}

func (t *trader) syncTrailguardBookStop(tc *config.TrailguardStrategyConfig, pos exchange.Position, res GuardResult) {
	if res.EffectiveFloor <= 0 {
		return
	}
	pairMeta, err := t.parent.Exchange.Pair(t.Pair)
	if err != nil {
		log.Printf("trailing book %s: pair meta: %v", t.Pair, err)
		return
	}
	tick := pairMeta.Base.TickSize
	triggerPx := roundToTick(res.EffectiveFloor, tick)
	if triggerPx <= 0 {
		return
	}
	if t.Trailguard.LastSyncedFloor > 0 && math.Abs(triggerPx-t.Trailguard.LastSyncedFloor) < tick/2 {
		return
	}

	t.cancelManagedExitStops(&pos)

	side := closeSideForPosition(pos)
	triggerType := trailguardBookTriggerType(tc)
	limitPx := triggerPx
	if triggerType == "limit" {
		if pos.Size > 0 {
			limitPx = roundToTick(triggerPx-tick, tick)
			if limitPx <= 0 {
				limitPx = triggerPx
			}
		} else {
			limitPx = roundToTick(triggerPx+tick, tick)
		}
	}

	orderType := "limit"
	if triggerType == "market" {
		orderType = "market"
	}
	ord := exchange.Order{
		Pair:         t.Pair,
		Side:         side,
		Type:         orderType,
		Size:         math.Abs(pos.Size),
		Price:        limitPx,
		TriggerPrice: triggerPx,
		TriggerType:  triggerType,
		ReduceOnly:   true,
		StopLoss:     true,
	}
	if err := t.parent.Exchange.PlaceOrders([]exchange.Order{ord}); err != nil {
		log.Printf("trailing book %s: place stop: %v", t.Pair, err)
		return
	}
	if id := findFreshStopOrderID(t.parent.Exchange, t.Pair, side, triggerPx); id != "" {
		t.Trailguard.BookStopOrderID = id
	}
	t.Trailguard.LastSyncedFloor = triggerPx
	log.Printf("trailing book %s: stop %s trigger=%.6f type=%s (%s)", t.Pair, side, triggerPx, triggerType, res.Reason)
}

func findFreshStopOrderID(exch exchange.I, pair, side string, trigger float64) string {
	for _, o := range exch.GetOrders() {
		if o.Pair != pair || o.Side != side || !o.ReduceOnly {
			continue
		}
		if o.TriggerPrice <= 0 || o.TakeProfit {
			continue
		}
		if math.Abs(o.TriggerPrice-trigger) < trigger*1e-9+1e-12 {
			return o.ID
		}
	}
	return ""
}
