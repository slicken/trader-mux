package model

import (
	"math"
	"strings"
	"ticktrader/exchange"
	"time"
)

func (t *trader) updatePair(pair *exchange.Pair) {
	if pair == nil {
		return
	}

	t.Lock()
	defer t.Unlock()

	t.MidPrice = pair.Price
	t.MarkPrice = pair.MarkPrice
	t.openInterest = pair.OpenInterest
	t.fundingRate = pair.FundingRate
	t.updateM1SMADistances()
}

func (t *trader) updatePrices(prices *[]exchange.Price) {
	if prices == nil || len(*prices) < 2 {
		return
	}

	bidPrice := (*prices)[0]
	askPrice := (*prices)[1]
	bid := bidPrice.Price
	ask := askPrice.Price
	if bid <= 0 || ask <= 0 || ask < bid {
		return
	}

	t.Lock()
	defer t.Unlock()

	t.bestBid = bid
	t.bestAsk = ask
	mid := (bid + ask) / 2
	spreadPct := ((ask - bid) / mid) * 100
	insertWithLimitInPlace(&t.Prices, [2]exchange.Price{bidPrice, askPrice}, ARRAY_SIZE)
	t.volatilityPct = EMA(calculateVolatilityPct(t.Prices, VOLATILITY_WINDOW), t.volatilityPct, VOLATILITY_EMA_ALPHA)
	t.latencyBufferPct = EMA(calculateLatencyBufferPct(t.parent.Exchange.GetLatency(), spreadPct), t.latencyBufferPct, LATENCY_EMA_ALPHA)
	t.spreadAvg = EMA(spreadPct, t.spreadAvg, SPREAD_EMA_ALPHA)
	t.updateM1SMADistances()
}

func calculateLatencyBufferPct(latencyMs int64, spreadPct float64) float64 {
	latencySeconds := float64(latencyMs) / 2 / 1000
	fixedBuffer := math.Max(latencySeconds*0.1, LATENCY_MIN_BUFFER_PCT)
	adaptiveBuffer := spreadPct * latencySeconds * 0.5
	return math.Max(fixedBuffer+adaptiveBuffer, LATENCY_MIN_BUFFER_PCT)
}

func calculateVolatilityPct(prices [][2]exchange.Price, window time.Duration) float64 {
	if len(prices) < 2 || window <= 0 {
		return 0
	}

	latestTime := pricePairTime(prices[0])
	if latestTime.IsZero() {
		return 0
	}
	cutoff := latestTime.Add(-window)

	var sumSq float64
	// We loop back from the newest price (index 0) to the cutoff
	for i := 1; i < len(prices); i++ {
		if pricePairTime(prices[i-1]).Before(cutoff) {
			break
		}

		newerMid := midPrice(prices[i-1])
		olderMid := midPrice(prices[i])

		if newerMid > 0 && olderMid > 0 {
			// Log returns capture the "energy" of each zig and zag
			logReturn := math.Log(newerMid / olderMid)
			sumSq += logReturn * logReturn
		}
	}

	// math.Sqrt(sumSq) aggregates the "energy" of all moves in the window.
	return math.Sqrt(sumSq) * 100
}

func volatilityRegime(volatilityPct float64) string {
	switch {
	case volatilityPct >= VOLATILITY_EXTREME_PCT:
		return "extreme"
	case volatilityPct >= VOLATILITY_HIGH_PCT:
		return "high"
	case volatilityPct >= VOLATILITY_LOW_PCT:
		return "normal"
	default:
		return "low"
	}
}

func midPrice(pricePair [2]exchange.Price) float64 {
	bid := pricePair[0].Price
	ask := pricePair[1].Price
	if bid <= 0 || ask <= 0 || ask < bid {
		return 0
	}
	return (bid + ask) / 2
}

func pricePairTime(pricePair [2]exchange.Price) time.Time {
	if !pricePair[0].Time.IsZero() {
		return pricePair[0].Time
	}
	return pricePair[1].Time
}

// smaSlopeRegime classifies 1m SMA slope (% change vs prior bar). Values: flat, up_normal, up_strong, down_normal, down_strong.
func smaSlopeRegime(slopePct float64) string {
	switch {
	case math.Abs(slopePct) <= SMA_SLOPE_FLAT_PCT:
		return "flat"
	case slopePct > SMA_SLOPE_STRONG_PCT:
		return "up_strong"
	case slopePct > SMA_SLOPE_FLAT_PCT:
		return "up_normal"
	case slopePct < -SMA_SLOPE_STRONG_PCT:
		return "down_strong"
	default:
		return "down_normal"
	}
}

func nearVolumeRegime(strength float64) string {
	switch {
	case strength >= ORDERBOOK_NEAR_EXTREME_PCT:
		return "extreme"
	case strength >= ORDERBOOK_NEAR_HIGH_PCT:
		return "high"
	case strength >= ORDERBOOK_NEAR_NORMAL_PCT:
		return "normal"
	default:
		return "low"
	}
}

func (t *trader) updateVolumes(orderbook *exchange.Orderbook) {
	bidsVol, asksVol, volumePct, bidNearNotional, askNearNotional, vpoc, vpRat := t.calculateOrderbook(orderbook, ORDERBOOK_LEVEL)

	t.Lock()
	defer t.Unlock()

	t.nearBidsVolumeAvg = EMA(bidNearNotional, t.nearBidsVolumeAvg, ORDERBOOK_NEAR_EMA_ALPHA)
	t.nearAsksVolumeAvg = EMA(askNearNotional, t.nearAsksVolumeAvg, ORDERBOOK_NEAR_EMA_ALPHA)
	// strenght calculation uses combined avg of bid and ask volumes as baseline
	base := (t.nearBidsVolumeAvg + t.nearAsksVolumeAvg) / 2
	if base <= 0 {
		t.nearBidsVolumeStr = 0
		t.nearAsksVolumeStr = 0
	} else {
		t.nearBidsVolumeStr = (bidNearNotional / base) * 100
		t.nearAsksVolumeStr = (askNearNotional / base) * 100
	}

	t.bidsVol = bidsVol
	t.asksVol = asksVol
	t.volumePct = volumePct
	t.vpoc = vpoc
	t.vpocRatio = vpRat
}

func (t *trader) calculateOrderbook(ob *exchange.Orderbook, levels int) (bidsVol, asksVol, imb, bidNear, askNear, vpoc, vpRat float64) {
	if ob == nil || levels <= 0 || len(ob.Bids) == 0 || len(ob.Asks) == 0 {
		return
	}

	mid := (ob.Bids[0].Price + ob.Asks[0].Price) / 2
	if mid <= 0 {
		return
	}

	tickSize := 0.0
	if t.parent != nil && t.parent.Exchange != nil {
		if pair, err := t.parent.Exchange.Pair(t.Pair); err == nil {
			tickSize = pair.Base.TickSize
		}
	}

	// decayed profile; zero or lower uses only the current orderbook snapshot.
	if t.vpocProfile.BucketSize <= 0 && tickSize > 0 {
		t.vpocProfile.BucketSize = math.Max(mid*(ORDERBOOK_VPOC_BUCKET_PCT/100), tickSize)
	}
	t.vpocProfile.DecayFactor = ORDERBOOK_VPOC_DECAY_FACTOR

	vpocEnabled := t.vpocProfile.BucketSize > 0
	var vpocBuckets map[int64]float64
	vpocBucketPrice := make(map[int64]exchange.Price)
	if vpocEnabled {
		if t.vpocProfile.DecayFactor > 0 {
			if t.vpocProfile.Buckets == nil {
				t.vpocProfile.Buckets = make(map[int64]float64)
			}
			for idx, volume := range t.vpocProfile.Buckets {
				t.vpocProfile.Buckets[idx] = volume * t.vpocProfile.DecayFactor
				if t.vpocProfile.Buckets[idx] <= 1e-8 {
					delete(t.vpocProfile.Buckets, idx)
				}
			}
			vpocBuckets = t.vpocProfile.Buckets
		} else {
			vpocBuckets = make(map[int64]float64)
		}
	}

	maxDistance := mid * (ORDERBOOK_DEPTH_PCT / 100)
	bidNearMinPrice := ob.Bids[0].Price * (1 - (ORDERBOOK_NEAR_DEPTH_PCT / 100))
	askNearMaxPrice := ob.Asks[0].Price * (1 + (ORDERBOOK_NEAR_DEPTH_PCT / 100))
	var weightedBidNotional, weightedAskNotional float64
	var actualBidNotional, actualAskNotional float64
	var bidNearNotional, askNearNotional float64

	process := func(bookLevels []exchange.Price, isBid bool) {
		for i := 0; i < levels && i < len(bookLevels); i++ {
			level := bookLevels[i]
			dist := math.Abs(level.Price - mid)

			if level.Price <= 0 || level.Size <= 0 || dist > maxDistance {
				if isBid && level.Price < (mid-maxDistance) {
					break
				}
				if !isBid && level.Price > (mid+maxDistance) {
					break
				}
				continue
			}

			weight := 1.0
			if ORDERBOOK_WEIGHT_FACTOR > 0 {
				weight = 1.0 / (1.0 + (dist/mid)*ORDERBOOK_WEIGHT_FACTOR*100)
			}

			// 3. Consistent Notional Imbalance
			notional := level.Price * level.Size
			weightedNotional := notional * weight
			if isBid {
				weightedBidNotional += weightedNotional
				actualBidNotional += notional
				if level.Price >= bidNearMinPrice {
					bidNearNotional += notional
				}
			} else {
				weightedAskNotional += weightedNotional
				actualAskNotional += notional
				if level.Price <= askNearMaxPrice {
					askNearNotional += notional
				}
			}

			if vpocEnabled {
				idx := int64(math.Floor(level.Price / t.vpocProfile.BucketSize))
				vpocBuckets[idx] += notional
				if levelNotional, exists := vpocBucketPrice[idx]; !exists || notional > levelNotional.Size {
					vpocBucketPrice[idx] = exchange.Price{Price: level.Price, Size: notional}
				}
			}
		}
	}

	process(ob.Bids, true)
	process(ob.Asks, false)

	totalWeightedNotional := weightedBidNotional + weightedAskNotional
	imbalance := 0.0
	if totalWeightedNotional > 0 {
		imbalance = ((weightedBidNotional - weightedAskNotional) / totalWeightedNotional) * 100.0
	}

	if !vpocEnabled {
		return actualBidNotional, actualAskNotional, imbalance, bidNearNotional, askNearNotional, 0, 0
	}

	// VPOC is the highest-volume price level inside the single highest-volume bucket.
	var bestIdx int64
	var maxBucketVolume float64
	for idx, volume := range vpocBuckets {
		if volume <= 1e-6 {
			continue
		}
		if volume > maxBucketVolume {
			maxBucketVolume = volume
			bestIdx = idx
		}
	}

	if maxBucketVolume <= 0 {
		return actualBidNotional, actualAskNotional, imbalance, bidNearNotional, askNearNotional, 0, 0
	}

	price := vpocBucketPrice[bestIdx].Price
	if price <= 0 {
		price = (float64(bestIdx) * t.vpocProfile.BucketSize) + (t.vpocProfile.BucketSize / 2)
	}

	vpRat = vpocBucketRatio(vpocBuckets, bestIdx)
	return actualBidNotional, actualAskNotional, imbalance, bidNearNotional, askNearNotional, price, vpRat
}

type VPOCProfile struct {
	Buckets     map[int64]float64
	BucketSize  float64
	DecayFactor float64
}

// vpocBucketRatio is notion(vpocIdx) / mean(two largest other bucket notionals in buckets).
// buckets is the same map used to pick vpocIdx (decayed profile or per-snapshot when decay off).
func vpocBucketRatio(buckets map[int64]float64, vpocIdx int64) float64 {
	if buckets == nil {
		return 0
	}
	top := buckets[vpocIdx]
	if top <= 1e-9 {
		return 0
	}
	var first, second float64
	for idx, vol := range buckets {
		if idx == vpocIdx || vol <= 1e-9 {
			continue
		}
		if vol > first {
			second = first
			first = vol
		} else if vol > second {
			second = vol
		}
	}
	if first <= 1e-9 {
		return 0
	}
	baseline := first
	if second > 1e-9 {
		baseline = (first + second) / 2
	}
	if baseline <= 1e-9 {
		return 0
	}
	return top / baseline
}

func vpocRegime(vpoc float64, vsNextTwoRatio float64) string {
	if vpoc <= 0 || vsNextTwoRatio <= 0 {
		return "normal"
	}
	switch {
	case vsNextTwoRatio >= ORDERBOOK_VPOC_EXTREME_RATIO:
		return "extreme"
	case vsNextTwoRatio >= ORDERBOOK_VPOC_HIGH_RATIO:
		return "high"
	case vsNextTwoRatio >= ORDERBOOK_VPOC_NORMAL_RATIO:
		return "normal"
	default:
		return "low"
	}
}

func (t *trader) updateTrade(trade *exchange.Trade) {
	if trade == nil {
		return
	}

	t.Lock()
	defer t.Unlock()

	insertWithLimitInPlace(&t.Trades, *trade, ARRAY_SIZE)
	t.tradePerMinute = t.calculateTradesInDuration(time.Minute)
	t.calculateSlippage(trade)
}

// calculateTradesInDuration calculates the number of stored trades inside duration.
// The caller must hold t.Lock or t.RLock.
func (t *trader) calculateTradesInDuration(duration time.Duration) int {
	if len(t.Trades) == 0 {
		return 0
	}

	var mostRecentTime time.Time
	var oldestTime time.Time
	for _, trade := range t.Trades {
		if trade.Order != nil && !trade.Order.Time.IsZero() {
			if mostRecentTime.IsZero() {
				mostRecentTime = trade.Order.Time
			}
			oldestTime = trade.Order.Time
		}
	}
	if mostRecentTime.IsZero() {
		return 0
	}

	cutoffTime := mostRecentTime.Add(-duration)
	count := 0
	for _, trade := range t.Trades {
		if trade.Order != nil && !trade.Order.Time.Before(cutoffTime) {
			count++
		}
	}

	// If the bounded trade buffer doesn't span the full duration, estimate
	// per-minute rate from the observed trade span to avoid undercounting.
	if len(t.Trades) == cap(t.Trades) && !oldestTime.IsZero() && oldestTime.After(cutoffTime) {
		spanSeconds := mostRecentTime.Sub(oldestTime).Seconds()
		if spanSeconds > 0 {
			return int(math.Round(float64(len(t.Trades)) * duration.Seconds() / spanSeconds))
		}
	}

	return count
}

// calculateSlippage records % distance from best ask (buys) or best bid (sells) to the worst fill (always non-negative).
// The caller must hold t.Lock.
func (t *trader) calculateSlippage(trade *exchange.Trade) {
	if trade == nil || len(trade.Fills) == 0 {
		return
	}

	side := ""
	if trade.Order != nil {
		switch strings.ToLower(trade.Order.Side) {
		case "buy", "sell":
			side = strings.ToLower(trade.Order.Side)
		}
	}

	var worst float64
	for _, fill := range trade.Fills {
		if fill == nil || fill.Price <= 0 || fill.Size <= 0 {
			continue
		}
		if side != "buy" && side != "sell" {
			switch strings.ToLower(fill.Side) {
			case "buy", "sell":
				side = strings.ToLower(fill.Side)
			}
		}
		switch side {
		case "buy":
			if fill.Price > worst {
				worst = fill.Price
			}
		case "sell":
			if worst == 0 || fill.Price < worst {
				worst = fill.Price
			}
		}
	}

	if (side != "buy" && side != "sell") || worst <= 0 {
		return
	}

	var ref float64
	if side == "sell" {
		ref = t.bestBid
	} else if side == "buy" {
		ref = t.bestAsk
	}
	if ref <= 0 {
		return
	}

	slippagePct := math.Abs((worst-ref)/ref) * 100
	insertWithLimitInPlace(&t.slippagePct, slippagePct, ARRAY_SIZE)
	t.updateSlippageAvg()
}

// updateSlippageAvg sets slippageAvg from slippagePct: arithmetic mean when SLIPPAGE_WEIGHT_FACTOR
// is 0 or >= 1; otherwise geometric weights with that factor (see model.SLIPPAGE_WEIGHT_FACTOR).
// The caller must hold t.Lock.
func (t *trader) updateSlippageAvg() {
	n := len(t.slippagePct)
	if n == 0 {
		t.slippageAvg = 0
		return
	}
	if n > ARRAY_SIZE {
		n = ARRAY_SIZE
	}

	f := SLIPPAGE_WEIGHT_FACTOR
	if f <= 0 || f >= 1 {
		var sum float64
		for i := 0; i < n; i++ {
			sum += t.slippagePct[i]
		}
		t.slippageAvg = sum / float64(n)
		return
	}

	var weightedSum float64
	var totalWeight float64
	for i := 0; i < n; i++ {
		weight := math.Pow(f, float64(n-i-1))
		weightedSum += t.slippagePct[i] * weight
		totalWeight += weight
	}
	if totalWeight <= 0 {
		t.slippageAvg = 0
		return
	}
	t.slippageAvg = weightedSum / totalWeight
}

func (t *trader) updateBar(bar *exchange.Bar) {
	if bar == nil {
		return
	}

	t.Lock()
	defer t.Unlock()

	previousSMA20 := t.m1_SMA20
	previousSMA200 := t.m1_SMA200
	t.updateBars(*bar)

	if sma := calculateSMA(t.Bars, 20); sma > 0 {
		t.m1_SMA20 = sma
		if previousSMA20 > 0 {
			t.m1_SMA20Slope = ((sma - previousSMA20) / previousSMA20) * 100
		}
	}
	if sma := calculateSMA(t.Bars, 200); sma > 0 {
		t.m1_SMA200 = sma
		if previousSMA200 > 0 {
			t.m1_SMA200Slope = ((sma - previousSMA200) / previousSMA200) * 100
		}
	}
	t.updateM1SMADistances()
}

// updateM1SMADistances sets signed % distance of mid (bid/ask midpoint, else exchange pair mid) from each M1 SMA.
// The caller must hold t.Lock.
func (t *trader) updateM1SMADistances() {
	mid := t.MidPrice
	if t.bestBid > 0 && t.bestAsk > 0 {
		mid = (t.bestBid + t.bestAsk) / 2
	}
	if mid <= 0 {
		t.m1_SMA20Distance = 0
		t.m1_SMA200Distance = 0
		return
	}
	if t.m1_SMA20 > 0 {
		t.m1_SMA20Distance = ((mid - t.m1_SMA20) / t.m1_SMA20) * 100
	} else {
		t.m1_SMA20Distance = 0
	}
	if t.m1_SMA200 > 0 {
		t.m1_SMA200Distance = ((mid - t.m1_SMA200) / t.m1_SMA200) * 100
	} else {
		t.m1_SMA200Distance = 0
	}
}

func (t *trader) updateBars(bar exchange.Bar) {
	if len(t.Bars) > 0 && t.Bars[0].Time.Equal(bar.Time) {
		t.Bars[0] = bar
		return
	}
	if len(t.Bars) > 0 && bar.Time.Before(t.Bars[0].Time) {
		return
	}

	insertWithLimitInPlace(&t.Bars, bar, ARRAY_SIZE)
}

func calculateSMA(bars []exchange.Bar, length int) float64 {
	if length <= 0 || len(bars) < length {
		return 0
	}

	var sum float64
	for i := 0; i < length; i++ {
		sum += bars[i].Close
	}
	return sum / float64(length)
}

func EMA(newValue, oldValue, alpha float64) float64 {
	if alpha <= 0 {
		return oldValue
	}
	if alpha >= 1 || oldValue == 0 {
		return newValue
	}
	return alpha*newValue + (1-alpha)*oldValue
}

func spreadRegime(spreadPct float64) string {
	switch {
	case spreadPct >= SPREAD_EXTREME_PCT:
		return "extreme"
	case spreadPct >= SPREAD_HIGH_PCT:
		return "high"
	case spreadPct >= SPREAD_LOW_PCT:
		return "normal"
	default:
		return "low"
	}
}
