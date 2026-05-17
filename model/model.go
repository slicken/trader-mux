package model

import (
	"context"
	"log"
	"math"
	"sync"
	"sync/atomic"
	"ticktrader/config"
	"ticktrader/exchange"
	"ticktrader/exchange/lighter"
	"time"
)

const (
	// pct values are real percentage values, not rations. (eg. 1 = 1%, 0.5 = 0.5%)
	ARRAY_SIZE             = 200  // Max number of recent bars/trades/prices to store in arrays
	BAR_INTERVAL           = "1m" // Resolution for bar (candlestick) data, e.g. "1m" = 1 minute bars
	SLIPPAGE_WEIGHT_FACTOR = 0.0  // 0 = simple average; 0 - 1 = exponential weighting; 1 = only most recent sample
	LATENCY_MIN_BUFFER_PCT = 0.01 // Minimum pct price buffer to account for latency when placing orders
	LATENCY_EMA_ALPHA      = 0.05 // EMA alpha for latencyBufferPct (calculateLatencyBufferPct in updatePrices)

	VOLATILITY_WINDOW      = 10 * time.Second // Lookback window for realized volatility calculation
	VOLATILITY_EMA_ALPHA   = 0.01             // EMA alpha for volatilityPct (calculateVolatilityPct in updatePrices)
	VOLATILITY_LOW_PCT     = 0.0005           // Threshold for "low" volatility regime, as a pct of price
	VOLATILITY_HIGH_PCT    = 0.005            // Threshold for "high" volatility regime, as a pct of price
	VOLATILITY_EXTREME_PCT = 0.015            // Threshold for "extreme" volatility regime, as a pct of price

	SPREAD_EMA_ALPHA   = 0.004 // EMA alpha for spreadAvg (instant bid-ask spread pct in updatePrices)
	SPREAD_LOW_PCT     = 0.005 // Threshold for "low" spread regime, as a pct of price
	SPREAD_HIGH_PCT    = 0.02  // Threshold for "high" spread regime, as a pct of price
	SPREAD_EXTREME_PCT = 0.05  // Threshold for "extreme" spread regime, as a pct of price

	SMA_SLOPE_FLAT_PCT   = 0.05 // Threshold for "flat" SMA slope regime, as |pct change vs prior 1m bar| (updateBar)
	SMA_SLOPE_STRONG_PCT = 0.22 // Threshold for "strong" SMA slope regime, as |pct change vs prior 1m bar| (updateBar)

	ORDERBOOK_LEVEL         = 200 // How many orderbook levels to pull/analyze when reading book data
	ORDERBOOK_DEPTH_PCT     = 0.5 // Only analyse orders within this percent depth from mid on each side
	ORDERBOOK_WEIGHT_FACTOR = 0.0 // 0 = Disabled. With ORDERBOOK_DEPTH_PCT=0.5, edge liquidity is weighted near 70%.

	ORDERBOOK_VPOC_BUCKET_PCT    = 0.01 // VPOC bucket width as percent of mid price
	ORDERBOOK_VPOC_DECAY_FACTOR  = 0.92 // 0 = Disabled. 1 = 100%
	ORDERBOOK_VPOC_NORMAL_RATIO  = 1.3  // ≥ this: normal-ish wall vs mean(second, third bucket); ratio < this → low (flat book)
	ORDERBOOK_VPOC_HIGH_RATIO    = 1.7  // Clearly stronger concentration than everyday structure
	ORDERBOOK_VPOC_EXTREME_RATIO = 2.5  // Rare: only standout huge walls / order clusters (raise further if extreme still too common)

	ORDERBOOK_NEAR_DEPTH_PCT   = 0.01 // How far from best bid/ask to analyze near-book volume, as a pct of price
	ORDERBOOK_NEAR_EMA_ALPHA   = 0.01 // EMA alpha for near-bids / near-asks notional baselines in updateVolumes
	ORDERBOOK_NEAR_NORMAL_PCT  = 35   // Threshold for normal near-volume regime (strength index); below = low
	ORDERBOOK_NEAR_HIGH_PCT    = 165  // Threshold for high near-volume regime (strength index)
	ORDERBOOK_NEAR_EXTREME_PCT = 250  // Threshold for extreme near-volume regime (strength index)
)

// Marketmaker is the main engine that manages exchange connection and global config
type Marketmaker struct {
	Exchange exchange.I
	config   *config.ModelConfig
	Cfg      *config.Config // full app cfg; Model is read for pair filter and reloads when dynamic config is on
	traders  map[string]*trader
	process  chan struct{} // Channel semaphore to ensure only one instance runs at a time
}

// trader is a pair-specific tradingg instance with its own world/settings
type trader struct {
	parent            *Marketmaker
	Pair              string
	Bars              []exchange.Bar
	Trades            []exchange.Trade
	Prices            [][2]exchange.Price
	slippagePct       []float64
	slippageAvg       float64
	spreadAvg         float64
	MarkPrice         float64
	MidPrice          float64
	bestBid           float64
	bestAsk           float64
	bidsVol           float64
	asksVol           float64
	volumePct         float64
	nearBidsVolumeStr float64
	nearBidsVolumeAvg float64
	nearAsksVolumeStr float64
	nearAsksVolumeAvg float64
	vpoc              float64
	vpocRatio         float64
	vpocProfile       VPOCProfile
	volatilityPct     float64
	latencyBufferPct  float64
	tradePerMinute    int
	openInterest      float64
	fundingRate       float64
	m1_SMA20          float64
	m1_SMA20Slope     float64
	m1_SMA20Distance  float64
	m1_SMA200         float64
	m1_SMA200Slope    float64
	m1_SMA200Distance float64
	// Trailguard is Pure Guard session state (dynamic stop); optional book-mode resting stop id.
	Trailguard TrailguardData
	sync.RWMutex
}

// Initialize creates the main engine and automatically adds pair traders for all pairs
func Initialize(exch exchange.I, cfg *config.ModelConfig) *Marketmaker {
	strat := &Marketmaker{
		Exchange: exch,
		config:   cfg,
		traders:  make(map[string]*trader),
		process:  make(chan struct{}, 1),
	}

	var pairs []string

	for _, pair := range exch.GetPairs() {
		if !pair.Enabled || !pair.IsPerp {
			continue
		}
		pairName := pair.Name

		// check if pairs are specified
		if len(cfg.Pairs) > 0 {
			found := false
			for _, p := range cfg.Pairs {
				if p == pairName {
					found = true
					break
				}
			}
			if !found {
				continue
			}

			pairs = append(pairs, pairName)
		}
	}

	for _, pair := range pairs {
		strat.traders[pair] = Newtrader(strat, pair)
	}

	var count atomic.Int64
	var wg sync.WaitGroup
	for _, pair := range pairs {
		wg.Add(1)

		go func(p string) {
			defer wg.Done()

			if err := exch.SubscribePair(p); err != nil {
				log.Printf("Error subscribing to pair %s: %v", p, err)
			} else {
				log.Printf("Subscribed %s to pair", p)
				count.Add(1)
			}
			if err := exch.SubscribeTrades(p); err != nil {
				log.Printf("Error subscribing to trades %s: %v", p, err)
			} else {
				log.Printf("Subscribed %s to trades", p)
				count.Add(1)
			}
			if err := exch.SubscribeBars(p, BAR_INTERVAL); err != nil {
				log.Printf("Error subscribing to %s bars %s: %v", p, BAR_INTERVAL, err)
			} else {
				log.Printf("Subscribed %s to bars %s", p, BAR_INTERVAL)
				count.Add(1)
			}
			if err := exch.SubscribePrices(p); err != nil {
				log.Printf("Error subscribing to prices %s: %v", p, err)
			} else {
				log.Printf("Subscribed %s to prices", p)
				count.Add(1)
			}
			if err := exch.SubscribeOrderbook(p); err != nil {
				log.Printf("Error subscribing to orderbook %s: %v", p, err)
			} else {
				log.Printf("Subscribed %s to orderbook", p)
				count.Add(1)
			}
			if err := strat.getInitialBars(p); err != nil {
				log.Printf("Error loading initial %s bars %s: %v", p, BAR_INTERVAL, err)
			}
		}(pair)
	}
	wg.Wait()

	log.Printf("Subscribed to %d pairs... (%d subscriptions)\n", len(pairs), count.Load())

	return strat
}

// Newtrader creates a new pair-specific trading instance
func Newtrader(parent *Marketmaker, pair string) *trader {
	return &trader{
		parent:            parent,
		Pair:              pair,
		Bars:              make([]exchange.Bar, 0, ARRAY_SIZE),
		Trades:            make([]exchange.Trade, 0, ARRAY_SIZE),
		Prices:            make([][2]exchange.Price, 0, ARRAY_SIZE),
		slippagePct:       make([]float64, 0, ARRAY_SIZE),
		slippageAvg:       0,
		spreadAvg:         0,
		MarkPrice:         0,
		MidPrice:          0,
		bestBid:           0,
		bestAsk:           0,
		bidsVol:           0,
		asksVol:           0,
		volumePct:         0,
		nearBidsVolumeStr: 0,
		nearBidsVolumeAvg: 0,
		nearAsksVolumeStr: 0,
		nearAsksVolumeAvg: 0,
		vpoc:              0,
		vpocRatio:         0,
		vpocProfile:       VPOCProfile{DecayFactor: ORDERBOOK_VPOC_DECAY_FACTOR},
		volatilityPct:     0,
		latencyBufferPct:  0,
		tradePerMinute:    0,
		openInterest:      0,
		fundingRate:       0,
		m1_SMA20:          0,
		m1_SMA20Slope:     0,
		m1_SMA20Distance:  0,
		m1_SMA200:         0,
		m1_SMA200Slope:    0,
		m1_SMA200Distance: 0,
	}
}

// Start the market maker and all its pair traders
func (strat *Marketmaker) Start(ctx context.Context) {
	log.Println("Model is running...")

	mainTicker := time.NewTicker(2 * time.Second)
	defer mainTicker.Stop()
	syncTicker := time.NewTicker(5 * time.Minute)
	defer syncTicker.Stop()

	// pd, err := strat.Exchange.Pair("BTC")
	// if err != nil {
	// 	log.Printf("Error getting BTCUSDT pair: %v", err)
	// }
	// log.Printf("%s %+v", pd.Base.Name, pd.Base.TickSize)
	// log.Printf("%s %+v", pd.Quote.Name, pd.Quote.TickSize)

	for {
		select {
		case <-ctx.Done():
			return
		case <-syncTicker.C:
			if exch, ok := strat.Exchange.(*lighter.Lighter); ok {
				if err := exch.UpdateOrders(); err != nil {
					log.Printf("Failed to update orders: %v", err)
				}
			}
		case bar := <-strat.Exchange.GetBarUpdates():
			if t := strat.traders[bar.Pair]; t != nil {
				t.updateBar(bar.Data)
			}
		case tr := <-strat.Exchange.GetTradeUpdates():
			if t := strat.traders[tr.Pair]; t != nil {
				t.updateTrade(tr.Data)
			}
		case pd := <-strat.Exchange.GetPairUpdates():
			if t := strat.traders[pd.Pair]; t != nil {
				t.updatePair(pd.Data)
			}
		case pu := <-strat.Exchange.GetPositionUpdates():
			strat.closeInstantProfitPosition(pu.Data)
		case pr := <-strat.Exchange.GetPricesUpdates():
			if t := strat.traders[pr.Pair]; t != nil {
				t.updatePrices(pr.Data)
			}
		case ob := <-strat.Exchange.GetOrderbookUpdates():
			if t := strat.traders[ob.Pair]; t != nil {
				t.updateVolumes(ob.Data)
			}

		// main ticker - update using channel semaphore
		case <-mainTicker.C:
			go func() {
				select {
				case strat.process <- struct{}{}:

					strat.update()

					// Release the process
					<-strat.process
				default:
				}
			}()
		}
	}
}

// Stop cancels all open orders and closes all positions for enabled pairs.
// Pairs listed in config DisabledPairs are skipped (no order cancel/position close).
func (strat *Marketmaker) Stop() {
	// Cancel open orders for enabled pairs only
	existingOrders := strat.Exchange.GetOrders()
	ordersToCancel := make([]exchange.Order, 0, len(existingOrders))
	for _, o := range existingOrders {
		ordersToCancel = append(ordersToCancel, o)
	}
	if len(ordersToCancel) > 0 {
		strat.Exchange.CancelOrders(ordersToCancel)
	}

	// Close positions with market reduce-only orders for enabled pairs only
	positions := strat.Exchange.GetPositions()
	closeOrders := make([]exchange.Order, 0, len(positions))
	for pair, pos := range positions {
		if pos.Size == 0 {
			continue
		}
		var side string
		switch {
		case pos.Size < 0:
			side = "buy"
		default:
			side = "sell"
		}
		closeOrders = append(closeOrders, exchange.Order{
			Pair:       pair,
			Side:       side,
			Type:       "market",
			Size:       math.Abs(pos.Size),
			Price:      0,
			ReduceOnly: true,
		})
	}
	if len(closeOrders) > 0 {
		strat.Exchange.PlaceOrders(closeOrders)
	}

	strat.traders = nil
	strat.Exchange = nil
	strat.config = nil
	strat.Exchange = nil
	strat.Cfg = nil
	log.Println("Marketmaker stopped!")
}

func (strat *Marketmaker) update() {
	// HTF MM: add entry / quoting logic here; manageOrders handles trailing for open positions only.
	strat.manageOrders()
}

// manageOrders runs the trailing engine for every pair that currently has size, and clears book stops
// for flat pairs that still have trailing bookkeeping.
func (strat *Marketmaker) manageOrders() {
	if strat.config == nil || strat.config.Trailguard == nil || !strat.config.Trailguard.Enabled {
		return
	}
	openPairs := make(map[string]struct{})
	for pair, pos := range strat.Exchange.GetPositions() {
		if pos.Size == 0 {
			continue
		}
		openPairs[pair] = struct{}{}
		if t := strat.traders[pair]; t != nil {
			t.manageOrders()
		}
	}
	for _, t := range strat.traders {
		if t == nil {
			continue
		}
		if _, ok := openPairs[t.Pair]; ok {
			continue
		}
		t.manageOrders()
	}
}

func (strat *Marketmaker) closeInstantProfitPosition(pos *exchange.Position) {
	if pos == nil || pos.Size == 0 {
		return
	}
	if strat.traders[pos.Pair] == nil {
		return
	}

	t := strat.traders[pos.Pair]
	t.RLock()
	bid, ask := t.bestBid, t.bestAsk
	t.RUnlock()

	entry := pos.AvgPrice
	if entry <= 0 || bid <= 0 || ask <= 0 || ask < bid {
		return
	}

	// Profitable at market touch: long exit is a sell vs bid; short exit is a buy vs ask.
	var ok bool
	switch {
	case pos.Size > 0:
		ok = bid > entry
	default:
		ok = ask < entry
	}
	if !ok {
		return
	}

	if strat.hasReduceOnlyCloseOrder(pos.Pair, pos.Size) {
		return
	}

	side := "sell"
	if pos.Size < 0 {
		side = "buy"
	}
	order := exchange.Order{
		Pair:       pos.Pair,
		Side:       side,
		Type:       "market",
		Size:       math.Abs(pos.Size),
		Price:      0,
		ReduceOnly: true,
	}

	if err := strat.Exchange.PlaceOrders([]exchange.Order{order}); err != nil {
		log.Printf("instant close: failed closing %s position %.6f bid=%.6f ask=%.6f entry=%.6f: %v", pos.Pair, pos.Size, bid, ask, entry, err)
		return
	}
	log.Printf("instant close: %s %s %.6f bid=%.6f ask=%.6f entry=%.6f PnL $%+.2f", pos.Pair, side, order.Size, bid, ask, entry, pos.PNL)
}

func (strat *Marketmaker) hasReduceOnlyCloseOrder(pair string, positionSize float64) bool {
	closeSide := "sell"
	if positionSize < 0 {
		closeSide = "buy"
	}
	for _, order := range strat.Exchange.GetOrders() {
		if order.Pair == pair && order.ReduceOnly && order.Side == closeSide && order.Status != "filled" && order.Status != "canceled" {
			return true
		}
	}
	return false
}

func (strat *Marketmaker) getInitialBars(pair string) error {
	bars, err := strat.Exchange.GetBars(pair, BAR_INTERVAL, ARRAY_SIZE)
	if err != nil {
		return err
	}

	t := strat.traders[pair]
	if t == nil {
		return nil
	}

	t.Lock()
	defer t.Unlock()

	t.Bars = t.Bars[:0]
	for _, bar := range bars {
		insertWithLimitInPlace(&t.Bars, bar, ARRAY_SIZE)
	}
	if sma := calculateSMA(t.Bars, 20); sma > 0 {
		t.m1_SMA20 = sma
	}
	if sma := calculateSMA(t.Bars, 200); sma > 0 {
		t.m1_SMA200 = sma
	}

	return nil
}
