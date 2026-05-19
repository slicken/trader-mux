package model

import (
	"math"
	"testing"
	"time"
	"ticktrader/exchange"
)

func TestDiffLevelSubmissionCancel(t *testing.T) {
	prev := map[float64]float64{100: 2, 99: 1}
	cur := map[float64]float64{100: 3, 98: 1}
	none := map[float64]float64{}

	sub, cancel := diffLevelSubmissionCancel(cur, prev, none)
	if sub != 198 {
		t.Fatalf("submission = %v, want 198", sub)
	}
	if cancel != 99 {
		t.Fatalf("cancellation = %v, want 99", cancel)
	}

	filled := map[float64]float64{100: 1}
	sub, cancel = diffLevelSubmissionCancel(map[float64]float64{100: 1}, map[float64]float64{100: 2}, filled)
	if sub != 0 {
		t.Fatalf("submission = %v, want 0", sub)
	}
	if cancel != 0 {
		t.Fatalf("cancellation = %v, want 0 (fill explains removal)", cancel)
	}

	filled = map[float64]float64{100: 0.5}
	sub, cancel = diffLevelSubmissionCancel(map[float64]float64{100: 1}, map[float64]float64{100: 2}, filled)
	if cancel != 50 {
		t.Fatalf("cancellation = %v, want 50", cancel)
	}
}

func TestTradeFillSizeByPrice(t *testing.T) {
	since := time.Now().Add(-time.Second)
	trades := []exchange.Trade{{
		Fills: []*exchange.Fill{{
			Side:  "sell",
			Price: 100,
			Size:  1.5,
			Time:  since.Add(100 * time.Millisecond),
		}},
	}}
	got := tradeFillSizeByPrice(trades, since, "sell", 0)
	if got[100] != 1.5 {
		t.Fatalf("fill size = %v", got[100])
	}
}

func TestCalculateOrderbookLevelMaps(t *testing.T) {
	tr := Newtrader(nil, "TEST")
	ob := &exchange.Orderbook{
		Bids: []exchange.Price{{Price: 100.5, Size: 1}, {Price: 100.4, Size: 2}},
		Asks: []exchange.Price{{Price: 100.6, Size: 1.5}},
	}

	tr.Lock()
	m := tr.calculateOrderbook(ob, 10, 0)
	prof := tr.orderbookLevelsProfile
	tr.Unlock()

	if !prof.ready {
		t.Fatal("expected ready profile")
	}
	if len(prof.bidSizes) != 2 || prof.bidSizes[100.5] != 1 || prof.bidSizes[100.4] != 2 {
		t.Fatalf("bidSizes = %#v", prof.bidSizes)
	}
	if len(prof.askSizes) != 1 || prof.askSizes[100.6] != 1.5 {
		t.Fatalf("askSizes = %#v", prof.askSizes)
	}
	if m.bidsVol != 100.5+100.4*2 {
		t.Fatalf("bidsVol = %v", m.bidsVol)
	}
	if math.Abs(m.asksVol-100.6*1.5) > 1e-9 {
		t.Fatalf("asksVol = %v", m.asksVol)
	}
}
