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

func midPrice(pricePair [2]exchange.Price) float64 {
	bid := pricePair[0].Price
	ask := pricePair[1].Price
	if bid <= 0 || ask <= 0 || ask < bid {
		return 0
	}
	return (bid + ask) / 2
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

	latestTime := prices[0][0].Time
	if latestTime.IsZero() {
		return 0
	}
	cutoff := latestTime.Add(-window)

	var sumSq float64
	// We loop back from the newest price (index 0) to the cutoff
	for i := 1; i < len(prices); i++ {
		if prices[i-1][0].Time.Before(cutoff) {
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

func (t *trader) updateVolumes(orderbook *exchange.Orderbook) {

	var curBidP, curBidSz, curAskP, curAskSz float64
	topOK := orderbook != nil && len(orderbook.Bids) > 0 && len(orderbook.Asks) > 0
	if topOK {
		curBidP = orderbook.Bids[0].Price
		curBidSz = orderbook.Bids[0].Size
		curAskP = orderbook.Asks[0].Price
		curAskSz = orderbook.Asks[0].Size
	}

	t.Lock()
	defer t.Unlock()

	bidsVol, asksVol, volumeImbalancePct, bidNearNotional, askNearNotional, vpoc, vpRat := t.calculateOrderbook(orderbook, ORDERBOOK_LEVEL)
	now := time.Now()

	prev := &t.volumeVelocityProfile
	newVel := t.volumeVelocity
	if topOK && prev.valid {
		if now.Sub(prev.at) >= time.Duration(ORDERBOOK_VELOCITY_MIN_GAP_MS)*time.Millisecond {
			tickSize := 0.0
			if t.parent != nil && t.parent.Exchange != nil {
				if pair, err := t.parent.Exchange.Pair(t.Pair); err == nil {
					tickSize = pair.Base.TickSize
				}
			}
			raw := topOfBookOFI(curBidP, curBidSz, curAskP, curAskSz, prev.bidPrice, prev.bidSize, prev.askPrice, prev.askSize, tickSize)
			newVel = EMA(raw, t.volumeVelocity, ORDERBOOK_VELOCITY_EMA_ALPHA)
			prev.bidPrice, prev.bidSize = curBidP, curBidSz
			prev.askPrice, prev.askSize = curAskP, curAskSz
			prev.at = now
		}
	} else if topOK {
		prev.bidPrice, prev.bidSize = curBidP, curBidSz
		prev.askPrice, prev.askSize = curAskP, curAskSz
		prev.at = now
		prev.valid = true
	} else {
		prev.valid = false
	}

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
	t.volumeImbalancePct = volumeImbalancePct
	t.vpoc = vpoc
	t.vpocRatio = vpRat
	t.volumeVelocity = newVel
}

func (t *trader) calculateOrderbook(ob *exchange.Orderbook, levels int) (bidsVol, asksVol, volumeImbalancePct, bidNear, askNear, vpoc, vpRat float64) {
	if ob == nil || levels <= 0 || len(ob.Bids) == 0 || len(ob.Asks) == 0 {
		return
	}

	mid := (ob.Bids[0].Price + ob.Asks[0].Price) / 2
	if mid <= 0 {
		return
	}

	// Tick size is only needed to derive BucketSize once; avoid Exchange.Pair on every snapshot.
	if t.vpocProfile.BucketSize <= 0 {
		tickSize := 0.0
		if t.parent != nil && t.parent.Exchange != nil {
			if pair, err := t.parent.Exchange.Pair(t.Pair); err == nil {
				tickSize = pair.Base.TickSize
			}
		}
		if tickSize > 0 {
			t.vpocProfile.BucketSize = math.Max(mid*(ORDERBOOK_VPOC_BUCKET_PCT/100), tickSize)
		}
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
	volumeImbalancePct = 0.0
	if totalWeightedNotional > 0 {
		volumeImbalancePct = ((weightedBidNotional - weightedAskNotional) / totalWeightedNotional) * 100.0
	}

	if !vpocEnabled {
		return actualBidNotional, actualAskNotional, volumeImbalancePct, bidNearNotional, askNearNotional, 0, 0
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
		return actualBidNotional, actualAskNotional, volumeImbalancePct, bidNearNotional, askNearNotional, 0, 0
	}

	price := vpocBucketPrice[bestIdx].Price
	if price <= 0 {
		price = (float64(bestIdx) * t.vpocProfile.BucketSize) + (t.vpocProfile.BucketSize / 2)
	}

	vpRat = vpocBucketRatio(vpocBuckets, bestIdx)
	return actualBidNotional, actualAskNotional, volumeImbalancePct, bidNearNotional, askNearNotional, price, vpRat
}

// topOfBookOFI is the Cont–Kukanov–Stoikov style increment from prev to cur best bid/ask, in quote notional (price×size, USD for USD-quoted perps).
// tickSize > 0 uses half-tick for "same price" vs improved bid / lowered ask.
func topOfBookOFI(curBidP, curBidSz, curAskP, curAskSz, prevBidP, prevBidSz, prevAskP, prevAskSz, tickSize float64) float64 {
	deltaBid := curBidSz - prevBidSz
	deltaAsk := curAskSz - prevAskSz
	var ofi float64
	eps := tickSize / 2
	sameBid := tickSize > 0 && math.Abs(curBidP-prevBidP) <= eps
	sameAsk := tickSize > 0 && math.Abs(curAskP-prevAskP) <= eps
	if tickSize <= 0 {
		sameBid = curBidP == prevBidP
		sameAsk = curAskP == prevAskP
	}
	if tickSize > 0 && curBidP > prevBidP+eps {
		ofi += curBidP * curBidSz
	} else if sameBid {
		ofi += curBidP * deltaBid
	} else if tickSize <= 0 && curBidP > prevBidP {
		ofi += curBidP * curBidSz
	}
	if tickSize > 0 && curAskP < prevAskP-eps {
		ofi -= curAskP * curAskSz
	} else if sameAsk {
		ofi -= curAskP * deltaAsk
	} else if tickSize <= 0 && curAskP < prevAskP {
		ofi -= curAskP * curAskSz
	}
	return ofi
}

// VolumeVelocityProfile holds the last accepted top-of-book snapshot for volumeVelocity (OFI).
type VolumeVelocityProfile struct {
	bidPrice float64
	askPrice float64
	bidSize  float64
	askSize  float64
	at       time.Time
	valid    bool
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

func (t *trader) updateTrade(trade *exchange.Trade) {
	if trade == nil {
		return
	}

	t.Lock()
	defer t.Unlock()

	insertWithLimitInPlace(&t.Trades, *trade, ARRAY_SIZE)

	perMin := calculateTradesInDurationFromTrades(t.Trades, time.Minute)
	now := time.Now()
	td, tdVol, timb, tfSec := aggregatedTradesFlowMetrics(t.Trades, now)

	bestBid, bestAsk := t.bestBid, t.bestAsk
	if sample, ok := calculateTradeSlippagePct(trade, bestBid, bestAsk); ok {
		newSlip := append([]float64(nil), t.slippagePct...)
		insertWithLimitInPlace(&newSlip, sample, ARRAY_SIZE)
		t.slippagePct = newSlip
		t.slippageAvg = slippageAvgFromSlice(newSlip)
	}

	t.tradesPerMinute = perMin
	t.tradesDelta = td
	t.tradesDeltaVolume = tdVol
	t.tradesImbalancePct = timb
	t.tradesFlowSec = tfSec
}

// aggregatedTradesFlowMetrics returns net buy-minus-sell fill count (tradesDelta), signed quote-notional
// buy-minus-sell (tradesDeltaVolume), taker notional imbalance %, and signed notional per second (tradesFlowSec) over TRADES_DELTA_WINDOW.
// Read-only over trades.
func aggregatedTradesFlowMetrics(trades []exchange.Trade, now time.Time) (tradesDelta, tradesDeltaVolume, tradesImbalancePct, tradesFlowSec float64) {
	cutoff := now.Add(-TRADES_DELTA_WINDOW)
	var buyN, sellN float64
	var buyCnt, sellCnt int
	for i := 0; i < len(trades); i++ {
		tr := &trades[i]
		for _, f := range tr.Fills {
			if f == nil || f.Size <= 0 || f.Price <= 0 {
				continue
			}
			if f.Time.Before(cutoff) {
				continue
			}
			side := strings.ToLower(f.Side)
			if side != "buy" && side != "sell" && tr.Order != nil {
				side = strings.ToLower(tr.Order.Side)
			}
			n := f.Price * f.Size
			switch side {
			case "buy":
				buyN += n
				buyCnt++
			case "sell":
				sellN += n
				sellCnt++
			}
		}
	}
	tradesDelta = float64(buyCnt - sellCnt)
	tradesDeltaVolume = buyN - sellN
	totalN := buyN + sellN
	if totalN > 0 {
		tradesImbalancePct = ((buyN - sellN) / totalN) * 100.0
	}
	sec := TRADES_DELTA_WINDOW.Seconds()
	if sec > 0 {
		tradesFlowSec = (buyN - sellN) / sec
	}
	return tradesDelta, tradesDeltaVolume, tradesImbalancePct, tradesFlowSec
}

// calculateTradesInDurationFromTrades is the same logic as the former calculateTradesInDuration, on a trades slice (read-only).
func calculateTradesInDurationFromTrades(trades []exchange.Trade, duration time.Duration) int {
	if len(trades) == 0 {
		return 0
	}

	var mostRecentTime time.Time
	var oldestTime time.Time
	for _, tr := range trades {
		if tr.Order != nil && !tr.Order.Time.IsZero() {
			if mostRecentTime.IsZero() {
				mostRecentTime = tr.Order.Time
			}
			oldestTime = tr.Order.Time
		}
	}
	if mostRecentTime.IsZero() {
		return 0
	}

	cutoffTime := mostRecentTime.Add(-duration)
	count := 0
	for _, tr := range trades {
		if tr.Order != nil && !tr.Order.Time.Before(cutoffTime) {
			count++
		}
	}

	if len(trades) >= ARRAY_SIZE && !oldestTime.IsZero() && oldestTime.After(cutoffTime) {
		spanSeconds := mostRecentTime.Sub(oldestTime).Seconds()
		if spanSeconds > 0 {
			return int(math.Round(float64(len(trades)) * duration.Seconds() / spanSeconds))
		}
	}

	return count
}

// calculateTradeSlippagePct returns one slippage sample % vs best bid/ask snapshot. Pure.
func calculateTradeSlippagePct(trade *exchange.Trade, bestBid, bestAsk float64) (float64, bool) {
	if trade == nil || len(trade.Fills) == 0 {
		return 0, false
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
		return 0, false
	}

	var ref float64
	if side == "sell" {
		ref = bestBid
	} else if side == "buy" {
		ref = bestAsk
	}
	if ref <= 0 {
		return 0, false
	}

	return math.Abs((worst-ref)/ref) * 100, true
}

// slippageAvgFromSlice matches updateSlippageAvg logic on a copy of slippagePct. Pure.
func slippageAvgFromSlice(samples []float64) float64 {
	n := len(samples)
	if n == 0 {
		return 0
	}
	if n > ARRAY_SIZE {
		n = ARRAY_SIZE
	}

	f := SLIPPAGE_WEIGHT_FACTOR
	if f <= 0 || f >= 1 {
		var sum float64
		for i := 0; i < n; i++ {
			sum += samples[i]
		}
		return sum / float64(n)
	}

	var weightedSum float64
	var totalWeight float64
	for i := 0; i < n; i++ {
		weight := math.Pow(f, float64(n-i-1))
		weightedSum += samples[i] * weight
		totalWeight += weight
	}
	if totalWeight <= 0 {
		return 0
	}
	return weightedSum / totalWeight
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
