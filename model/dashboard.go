package model

import (
	"context"
	"encoding/json"
	"html/template"
	"log"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"ticktrader/exchange"
	"time"
)

const (
	DASHBOARD_CHART_WINDOW     = 20 * time.Second       // chart window duration
	DASHBOARD_REFRESH_INTERVAL = 100 * time.Millisecond // interval between dashboard refreshes
	CHART_SCALE_RATIO          = 0.0                    // scale ratio for charts (0.0 = no scaling, 1.0 = full scaling)
)

type Dashboard struct {
	addr      string
	server    *http.Server
	model     *Marketmaker
	histories map[string]*dashboardHistory
	mu        sync.Mutex
}

type dashboardHistory struct {
	// PairedQuotes keeps each bid with the ask from the same exchange update.
	// Merging bids and asks in separate slices breaks index alignment and makes charts look spiky.
	PairedQuotes   [][2]exchange.Price
	MarkPrices     []exchange.Price
	M1SMA20Prices  []exchange.Price
	M1SMA200Prices []exchange.Price
	Trades         []dashboardTradePoint
}

type DashboardData struct {
	PairsData          []DashboardPairData `json:"pairs_data"`
	LastUpdate         time.Time           `json:"last_update"`
	LatencyMs          map[string]int64    `json:"latency_ms"`
	ChartWindowSeconds int                 `json:"chart_window_seconds"`
}

type dashboardTemplateData struct {
	ChartWindowSeconds   int
	RefreshMs            int
	ChartScaleRatio      float64
	NearDepthPct         float64
	OrderbookDepthPct    float64
	TradesDeltaWindowSec int
	VolatilityWindowSec  int
}

type DashboardPairData struct {
	Exchange             string                `json:"exchange"`
	Symbol               string                `json:"symbol"`
	PriceDecimals        int                   `json:"price_decimals"`
	MarkPrice            float64               `json:"mark_price"`
	MidPrice             float64               `json:"mid_price"`
	SpreadAvg            float64               `json:"spread_avg"`
	Spread               float64               `json:"spread"`
	SpreadRegime         string                `json:"spread_regime"`
	SlippageAvg          float64               `json:"slippage_avg"`
	VPOC                 float64               `json:"vpoc"`
	VpocRatio            float64               `json:"vpoc_ratio"`
	VpocRegime           string                `json:"vpoc_regime"`
	VolumeImbalancePct   float64               `json:"volume_imbalance_pct"`
	TopVolumeVelocity    float64               `json:"top_volume_velocity"` // EMA of top-of-book OFI (quote notional, e.g. USD)
	BidsDeltaVelocity      float64               `json:"bids_delta_velocity"` // ORDERBOOK_DEPTH_PCT net submission − cancellation $/s (EMA)
	AsksDeltaVelocity      float64               `json:"asks_delta_velocity"`
	NearBidsDeltaVelocity  float64               `json:"near_bids_delta_velocity"` // ORDERBOOK_NEAR_DEPTH_PCT
	NearAsksDeltaVelocity  float64               `json:"near_asks_delta_velocity"`
	NearBidsVolumeStr      float64               `json:"near_bids_volume_str"`
	NearBidsVolumeAvg    float64               `json:"near_bids_volume_avg"`
	NearBidsVolumeRegime string                `json:"near_bids_volume_regime"`
	NearAsksVolumeStr    float64               `json:"near_asks_volume_str"`
	NearAsksVolumeAvg    float64               `json:"near_asks_volume_avg"`
	NearAsksVolumeRegime string                `json:"near_asks_volume_regime"`
	VolatilityPct        float64               `json:"volatility_pct"`
	VolatilityRegime     string                `json:"volatility_regime"`
	TradesPerMinute      int                   `json:"trades_per_minute"`
	TradesFlowSec          float64               `json:"trades_flow_sec"`         // signed taker notional / s (avg over TRADES_DELTA_WINDOW)
	TradesImbalancePct   float64               `json:"trades_imbalance_pct"`  // taker buy vs sell notional imbalance % in window
	TradesDelta          float64               `json:"trades_delta"`          // net buy fills minus sell fills in window (count)
	TradesDeltaVolume    float64               `json:"trades_delta_volume"`   // signed quote notional (buy - sell) in window
	OpenInterest         int64                 `json:"open_interest"`
	FundingRate          float64               `json:"funding_rate"`
	M1_SMA20             float64               `json:"m1_sma20"`
	M1_SMA20Slope        float64               `json:"m1_sma20_slope"`
	M1_SMA20SlopeRegime  string                `json:"m1_sma20_slope_regime"`
	M1_SMA20Distance     float64               `json:"m1_sma20_distance"`
	M1_SMA200            float64               `json:"m1_sma200"`
	M1_SMA200Slope       float64               `json:"m1_sma200_slope"`
	M1_SMA200SlopeRegime string                `json:"m1_sma200_slope_regime"`
	M1_SMA200Distance    float64               `json:"m1_sma200_distance"`
	BidPrices            []exchange.Price      `json:"bid_prices"`
	AskPrices            []exchange.Price      `json:"ask_prices"`
	MarkPrices           []exchange.Price      `json:"mark_prices"`
	M1SMA20Prices        []exchange.Price      `json:"m1_sma20_prices"`
	M1SMA200Prices       []exchange.Price      `json:"m1_sma200_prices"`
	OBBidLevels          []exchange.Price      `json:"ob_bid_levels"`
	OBAskLevels          []exchange.Price      `json:"ob_ask_levels"`
	OBMinPrice           float64               `json:"ob_min_price"`
	OBMaxPrice           float64               `json:"ob_max_price"`
	Trades               []dashboardTradePoint `json:"trades"`
}

type dashboardTradePoint struct {
	Price      float64
	StartPrice float64
	EndPrice   float64
	Size       float64
	Time       time.Time
	Side       string
}

func NewDashboard(model *Marketmaker, addr string) *Dashboard {
	return &Dashboard{
		model:     model,
		addr:      addr,
		histories: make(map[string]*dashboardHistory),
	}
}

// effectiveDashboardPrefs merges config.json dashboard.* with defaults from this package.
// Chart scale follows config whenever chart_scale_ratio is non-zero (0 omits scaling = same as builtin default).
func (d *Dashboard) effectiveDashboardPrefs() (chartWindowSec int, refreshMs int, chartScaleRatio float64) {
	chartWindowSec = int(DASHBOARD_CHART_WINDOW / time.Second)
	refreshMs = int(DASHBOARD_REFRESH_INTERVAL / time.Millisecond)
	chartScaleRatio = CHART_SCALE_RATIO

	if strat := d.model; strat != nil && strat.Cfg != nil {
		dc := strat.Cfg.Dashboard
		if dc.ChartScaleRatio != 0 {
			chartScaleRatio = dc.ChartScaleRatio
		}
		if s := strings.TrimSpace(dc.ChartWindow); s != "" {
			if dur, err := time.ParseDuration(s); err == nil && dur > 0 {
				sec := int(dur / time.Second)
				if sec < 1 {
					sec = 1
				}
				chartWindowSec = sec
			}
		}
		if s := strings.TrimSpace(dc.RefreshInterval); s != "" {
			if dur, err := time.ParseDuration(s); err == nil && dur > 0 {
				ms := int(dur / time.Millisecond)
				if ms < 50 {
					ms = 50
				}
				refreshMs = ms
			}
		}
	}
	return chartWindowSec, refreshMs, chartScaleRatio
}

func (d *Dashboard) chartWindowDuration() time.Duration {
	sec, _, _ := d.effectiveDashboardPrefs()
	if sec < 1 {
		return DASHBOARD_CHART_WINDOW
	}
	return time.Duration(sec) * time.Second
}

func (d *Dashboard) Start(ctx context.Context) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", d.dashboardHandler)
	mux.HandleFunc("/api/data", d.apiHandler)

	d.server = &http.Server{
		Addr:    d.addr,
		Handler: mux,
	}

	go func() {
		if err := d.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("Dashboard failed: %v", err)
		}
	}()

	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.server.Shutdown(shutdownCtx); err != nil {
		log.Printf("Dashboard shutdown failed: %v", err)
	}
}

func (d *Dashboard) dashboardHandler(w http.ResponseWriter, r *http.Request) {
	tmpl := `
<!DOCTYPE html>
<html>
<head>
	<title>Ticktrader Dashboard</title>
	<meta charset="utf-8">
	<script src="https://cdn.jsdelivr.net/npm/chart.js"></script>
	<style>
		body {
			font-family: 'Courier New', 'Monaco', 'Menlo', 'Consolas', monospace;
			margin: 0;
			padding: 12px;
			background-color: #1a1a1a;
			color: #ffffff;
			overflow: auto;
		}
		.meta {
			color: #9ca3af;
			margin-bottom: 8px;
			font-size: 12px;
		}
		.pair-grid {
			display: grid;
			gap: 8px;
			min-height: calc(100vh - 44px);
			--metric-scale: 1;
			--metric-grid-columns: repeat(auto-fit, minmax(112px, 1fr));
			--metrics-column: 34%;
		}
		.pair-section {
			display: grid;
			grid-template-columns: minmax(420px, var(--metrics-column)) minmax(0, 1fr);
			gap: 8px;
			background: #1a1a1a;
			border-radius: 8px;
			padding: clamp(8px, calc(8px * var(--metric-scale)), 14px);
			min-height: 0;
			overflow: hidden;
		}
		.metrics-panel {
			display: flex;
			flex-direction: column;
			gap: clamp(6px, calc(6px * var(--metric-scale)), 12px);
			min-width: 0;
			min-height: 0;
			overflow: auto;
		}
		.pair-title {
			display: flex;
			align-items: baseline;
			justify-content: space-between;
			gap: 10px;
		}
		.symbol {
			font-size: clamp(13px, calc(14px * var(--metric-scale)), 30px);
			font-weight: 700;
		}
		.metric-grid {
			display: grid;
			grid-template-columns: var(--metric-grid-columns);
			gap: clamp(6px, calc(6px * var(--metric-scale)), 12px);
			min-height: 0;
		}
		.metric {
			background: #2a2a2a;
			border-radius: 6px;
			padding: clamp(6px, calc(6px * var(--metric-scale)), 12px);
			min-width: 0;
		}
		.metric-label {
			color: #9ca3af;
			font-size: clamp(8px, calc(8px * var(--metric-scale)), 15px);
			text-transform: uppercase;
			margin-bottom: clamp(2px, calc(2px * var(--metric-scale)), 5px);
		}
		.metric-label-row {
			display: flex;
			align-items: center;
			justify-content: space-between;
			gap: 8px;
		}
		.metric-value {
			font-size: clamp(11px, calc(12px * var(--metric-scale)), 28px);
			font-weight: 700;
			overflow: hidden;
			text-overflow: ellipsis;
			white-space: nowrap;
		}
		.metric-value.compact {
			font-size: clamp(9px, calc(9px * var(--metric-scale)), 18px);
		}
		.near-volume-avg-line {
			margin-top: clamp(2px, calc(3px * var(--metric-scale)), 5px);
		}
		.chart-panel {
			display: flex;
			flex-direction: column;
			min-width: 0;
			min-height: 0;
		}
		.chart-wrap {
			position: relative;
			flex: 1;
			min-height: 0;
		}
		.chart-points {
			position: absolute;
			top: 6px;
			right: 8px;
			z-index: 2;
			color: #9ca3af;
			font-size: clamp(9px, calc(10px * var(--metric-scale)), 12px);
			background: rgba(26, 26, 26, 0.55);
			border: 1px solid rgba(156, 163, 175, 0.18);
			border-radius: 6px;
			padding: 3px 7px;
			pointer-events: none;
			white-space: nowrap;
		}
		.regime-legend {
			display: inline-flex;
			gap: 3px;
			flex: 0 0 auto;
		}
		.regime-chip {
			width: 8px;
			height: 8px;
			border-radius: 2px;
			opacity: 0.9;
		}
		.regime-chip.regime-low { background: #4ade80; }
		.regime-chip.regime-normal { background: #d1d5db; }
		.regime-chip.regime-high { background: #facc15; }
		.regime-chip.regime-extreme { background: #f87171; }
		.depth-overlay {
			position: absolute;
			top: 0;
			right: 18px;
			bottom: 0;
			width: 44%;
			pointer-events: none;
			z-index: 1;
		}
		.depth-midline {
			position: absolute;
			top: 0;
			bottom: 0;
			right: 14px;
			width: 1px;
			background: rgba(156, 163, 175, 0.35);
		}
		.depth-level {
			position: absolute;
			right: 14px;
			height: 20px;
			opacity: 0.3;
			border-radius: 2px;
		}
		.depth-bid {
			background: rgba(74, 222, 128, 0.3);
		}
		.depth-ask {
			background: rgba(248, 113, 113, 0.3);
		}
		.near-marker {
			position: absolute;
			right: 14px;
			width: 38px;
			height: 0;
			border-top: 2px solid;
			opacity: 0.9;
		}
		.near-marker-boundary {
			opacity: 0.65;
		}
		.near-marker-bid {
			border-color: #4ade80;
		}
		.near-marker-ask {
			border-color: #f87171;
		}
		.positive { color: #4ade80; }
		.negative { color: #f87171; }
		.neutral { color: #d1d5db; }
		.regime-low { color: #4ade80; }
		.regime-normal { color: #d1d5db; }
		.regime-high { color: #facc15; }
		.regime-extreme { color: #f87171; }
		@media (max-width: 1100px) {
			.pair-grid {
				height: auto;
				overflow: visible;
			}
			body { overflow: auto; }
			.pair-section {
				grid-template-columns: 1fr;
				min-height: 420px;
			}
			.chart-wrap {
				min-height: 260px;
			}
		}
	</style>
</head>
<body>
	<div class="meta" id="meta">Connecting...</div>
	<div class="pair-grid" id="pair-grid"></div>

	<script>
		const fmt = (value, digits = 6) => Number(value || 0).toFixed(digits);
		const priceDigits = row => Number.isInteger(row?.price_decimals) ? row.price_decimals : 6;
		const fmtPrice = (value, digits = 6) => fmt(value, digits);
		const pct = (value, digits = 4) => fmt(value, digits) + '%';
		const cls = value => value > 0 ? 'positive' : value < 0 ? 'negative' : 'neutral';
		function fmtDeltaPerSec(value) {
			const v = Number(value || 0);
			const a = Math.abs(v);
			if (a === 0) return '0';
			if (a < 0.0001) return v.toFixed(8);
			if (a < 0.01) return v.toFixed(6);
			if (a < 1) return v.toFixed(4);
			if (a < 100) return v.toFixed(2);
			return v.toFixed(0);
		}
		function smaSlopePctValueClass(regime) {
			switch (regime) {
			case 'up_strong':
			case 'down_strong':
				return regimeClass('high');
			default:
				return 'neutral';
			}
		}
		const DASHBOARD_CHART_WINDOW_SECONDS = {{ .ChartWindowSeconds }};
		const DASHBOARD_REFRESH_MS = {{ .RefreshMs }};
		const CHART_SCALE_RATIO = {{ .ChartScaleRatio }};
		const ORDERBOOK_NEAR_DEPTH_PCT = {{ .NearDepthPct }};
		const ORDERBOOK_DEPTH_PCT = {{ .OrderbookDepthPct }};
		const TRADES_DELTA_WINDOW_SEC = {{ .TradesDeltaWindowSec }};
		const VOLATILITY_WINDOW_SEC = {{ .VolatilityWindowSec }};
		let chartWindowSeconds = DASHBOARD_CHART_WINDOW_SECONDS;
		let charts = {};
		let renderedKeys = [];

		function rowKey(row) {
			return row.exchange + ':' + row.symbol;
		}

		function chartOptions() {
			return {
				type: 'line',
				data: {
					labels: [],
					datasets: [{
						label: 'bid',
						data: [],
						borderColor: '#4ade80',
						backgroundColor: 'rgba(74, 222, 128, 0.10)',
						borderWidth: 1.6,
						pointRadius: 0,
						pointHoverRadius: 3,
						tension: 0,
					}, {
						label: 'ask',
						data: [],
						borderColor: '#f87171',
						backgroundColor: 'rgba(248, 113, 113, 0.10)',
						borderWidth: 1.6,
						pointRadius: 0,
						pointHoverRadius: 3,
						tension: 0,
					}, {
						label: 'mark',
						data: [],
						borderColor: '#9ca3af',
						backgroundColor: 'rgba(156, 163, 175, 0.10)',
						borderWidth: 1.3,
						pointRadius: 0,
						pointHoverRadius: 0,
						tension: 0,
					}, {
						label: 'SMA20',
						data: [],
						borderColor: '#1d4ed8',
						backgroundColor: 'rgba(29, 78, 216, 0.10)',
						borderWidth: 1.4,
						pointRadius: 0,
						pointHoverRadius: 0,
						tension: 0,
					}, {
						label: 'SMA200',
						data: [],
						borderColor: '#f23645',
						backgroundColor: 'rgba(242, 54, 69, 0.10)',
						borderWidth: 1.4,
						pointRadius: 0,
						pointHoverRadius: 0,
						tension: 0,
					}, {
						label: 'VPOC',
						data: [],
						borderColor: '#dbdbdb',
						backgroundColor: 'rgba(219, 219, 219, 0.10)',
						borderWidth: 1.4,
						pointRadius: 0,
						pointHoverRadius: 0,
						tension: 0,
					}, {
						label: 'trades',
						data: [],
						borderColor: 'rgba(255, 255, 255, 0.7)',
						backgroundColor: '#ffffff',
						borderWidth: 1,
						pointRadius: 0,
						pointHoverRadius: 0,
						showLine: false,
					}],
				},
				options: {
					responsive: true,
					maintainAspectRatio: false,
					animation: false,
					interaction: { mode: 'nearest', intersect: false },
					plugins: {
						legend: {
							align: 'start',
							labels: { color: '#d1d5db' },
						},
						tooltip: {
							callbacks: {
								label: ctx => {
									const digits = Number.isInteger(ctx.chart.priceDecimals) ? ctx.chart.priceDecimals : 6;
									const base = ctx.dataset.label + ': ' + fmtPrice(ctx.parsed.y, digits);
									if (ctx.dataset.label !== 'trades') {
										return base;
									}
									const start = ctx.dataset.tradeStarts && ctx.dataset.tradeStarts[ctx.dataIndex];
									const end = ctx.dataset.tradeEnds && ctx.dataset.tradeEnds[ctx.dataIndex];
									const sz = ctx.dataset.tradeSizes && ctx.dataset.tradeSizes[ctx.dataIndex];
									const parts = [];
									if (sz != null && Number.isFinite(sz) && sz > 0) {
										parts.push('size ' + fmt(sz, 6));
									}
									if (
										start != null &&
										end != null &&
										Number.isFinite(start) &&
										Number.isFinite(end) &&
										start !== end
									) {
										parts.push('slippage ' + fmtPrice(start, digits) + ' -> ' + fmtPrice(end, digits));
									}
									return parts.length > 0 ? base + ' | ' + parts.join(' | ') : base;
								},
							},
						},
					},
					scales: {
						x: {
							ticks: { color: '#9ca3af', maxTicksLimit: 8 },
							grid: { color: 'rgba(156, 163, 175, 0.12)' },
						},
						y: {
							position: 'right',
							ticks: {
								color: '#9ca3af',
								callback: function(value) {
									const digits = Number.isInteger(this.chart.priceDecimals) ? this.chart.priceDecimals : 6;
									return fmtPrice(value, digits);
								},
							},
							grid: { color: 'rgba(156, 163, 175, 0.12)' },
						},
					},
				},
			};
		}

		function clamp(min, value, max) {
			return Math.max(min, Math.min(value, max));
		}

		function chartWindowLabel() {
			if (chartWindowSeconds >= 60 && chartWindowSeconds % 60 === 0) {
				return (chartWindowSeconds / 60) + 'm';
			}
			return chartWindowSeconds + 's';
		}

		function maxTimeMs(series) {
			let max = 0;
			for (const point of series) {
				const ms = new Date(point.Time).getTime();
				if (!Number.isFinite(ms)) {
					continue;
				}
				if (ms > max) {
					max = ms;
				}
			}
			return max;
		}

		function trimPricesByWindow(series, cutoffMs) {
			return series.filter(point => new Date(point.Time).getTime() >= cutoffMs);
		}

		function trimRollingChartWindow(bids, asks, marks, m1sma20s, m1sma200s, trades) {
			const windowMs = chartWindowSeconds * 1000;
			const endMs = Math.max(
				maxTimeMs(bids),
				maxTimeMs(asks),
				maxTimeMs(marks),
				maxTimeMs(m1sma20s),
				maxTimeMs(m1sma200s),
				maxTimeMs(trades),
			);
			if (!Number.isFinite(endMs) || endMs <= 0) {
				return { bids, asks, marks, m1sma20s, m1sma200s, trades };
			}

			const cutoffMs = endMs - windowMs;
			let nextBids = trimPricesByWindow(bids, cutoffMs);
			let nextAsks = trimPricesByWindow(asks, cutoffMs);
			const nextMarks = trimPricesByWindow(marks, cutoffMs);
			const nextM1Sma20s = trimPricesByWindow(m1sma20s, cutoffMs);
			const nextM1Sma200s = trimPricesByWindow(m1sma200s, cutoffMs);
			const nextTrades = trimPricesByWindow(trades, cutoffMs);

			const pairCount = Math.min(nextBids.length, nextAsks.length);
			nextBids = nextBids.slice(-pairCount);
			nextAsks = nextAsks.slice(-pairCount);

			return {
				bids: nextBids,
				asks: nextAsks,
				marks: nextMarks,
				m1sma20s: nextM1Sma20s,
				m1sma200s: nextM1Sma200s,
				trades: nextTrades,
			};
		}

		function visibleChartRows(rowCount) {
			return Math.min(Math.max(rowCount || 1, 1), 2);
		}

		function dashboardHeightPx() {
			return Math.max(window.innerHeight - 44, 240);
		}

		function chartRowHeightPx(rowCount) {
			const visibleRows = visibleChartRows(rowCount);
			const rowGap = 8;
			const availableHeight = dashboardHeightPx() - (rowGap * (visibleRows - 1));
			return Math.max(180, Math.floor(availableHeight / visibleRows));
		}

		function applyChartRowSizing(rowCount) {
			const grid = document.getElementById('pair-grid');
			const count = Math.max(rowCount || renderedKeys.length || 1, 1);
			const rowHeight = chartRowHeightPx(count);
			grid.style.gridTemplateRows = count > 0
				? 'repeat(' + count + ', minmax(0, ' + rowHeight + 'px))'
				: '1fr';
		}

		function adaptMetricSizing(rowCount) {
			const grid = document.getElementById('pair-grid');
			const count = Math.max(rowCount || renderedKeys.length || 1, 1);
			const rowHeight = chartRowHeightPx(count);
			const scale = clamp(0.95, rowHeight / 230, 2.05);
			const minWidth = Math.round(clamp(112, 112 + ((scale - 1) * 70), 190));
			const metricColumns = rowHeight >= 250 ? 'repeat(3, minmax(0, 1fr))' : 'repeat(auto-fit, minmax(' + minWidth + 'px, 1fr))';
			const metricsColumn = scale > 1.6 ? '44%' : scale > 1.25 ? '40%' : '34%';

			grid.style.setProperty('--metric-scale', scale.toFixed(2));
			grid.style.setProperty('--metric-grid-columns', metricColumns);
			grid.style.setProperty('--metrics-column', metricsColumn);
		}

		function metric(label, value, className) {
			return '<div class="metric">' +
				'<div class="metric-label">' + label + '</div>' +
				'<div class="metric-value ' + (className || 'neutral') + '">' + value + '</div>' +
			'</div>';
		}

		function compactMetric(label, value, className) {
			return '<div class="metric">' +
				'<div class="metric-label">' + label + '</div>' +
				'<div class="metric-value compact ' + (className || 'neutral') + '">' + value + '</div>' +
			'</div>';
		}

		function regimeMetric(label, value, regime) {
			return '<div class="metric">' +
				'<div class="metric-label metric-label-row"><span>' + label + '</span>' + regimeLegendHtml() + '</div>' +
				'<div class="metric-value ' + regimeClass(regime) + '">' + value + '</div>' +
			'</div>';
		}

		function regimeClass(regime) {
			return 'regime-' + (regime || 'low');
		}

		function regimeLegendHtml() {
			return '<span class="regime-legend">' +
				'<span class="regime-chip regime-low"></span>' +
				'<span class="regime-chip regime-normal"></span>' +
				'<span class="regime-chip regime-high"></span>' +
				'<span class="regime-chip regime-extreme"></span>' +
			'</span>';
		}

		function nearVolumeBox(label, strVal, avgVal, regime) {
			return '<div class="metric">' +
				'<div class="metric-label metric-label-row"><span>' + label + '</span>' + regimeLegendHtml() + '</div>' +
				'<div class="metric-value ' + regimeClass(regime) + '">' + pct(strVal) + '</div>' +
				'<div class="metric-value compact neutral near-volume-avg-line">avg ' + fmt(avgVal, 2) + '</div>' +
			'</div>';
		}

		function spreadBox(label, spreadVal, spreadAvg, regime) {
			return '<div class="metric">' +
				'<div class="metric-label metric-label-row"><span>' + label + '</span>' + regimeLegendHtml() + '</div>' +
				'<div class="metric-value ' + regimeClass(regime) + '">' + pct(spreadVal) + '</div>' +
				'<div class="metric-value compact neutral near-volume-avg-line">avg ' + pct(spreadAvg) + '</div>' +
			'</div>';
		}

		function smaBox(label, priceVal, slopeVal, slopeRegime, distanceVal, digits) {
			const slopeLine = '<div class="metric-value compact ' + smaSlopePctValueClass(slopeRegime) + ' near-volume-avg-line">slope ' + pct(slopeVal) + '</div>';
			const distanceLine = priceVal > 0
				? '<div class="metric-value compact neutral near-volume-avg-line">dist ' + pct(distanceVal) + '</div>'
				: '';
			return '<div class="metric">' +
				'<div class="metric-label">' + label + '</div>' +
				'<div class="metric-value ' + cls(slopeVal) + '">' + fmtPrice(priceVal, digits) + '</div>' +
				slopeLine +
				distanceLine +
				'</div>';
		}

		function vpocBox(priceVal, ratioVal, regime, digits) {
			const val = priceVal > 0 ? fmtPrice(priceVal, digits) : '—';
			const r = Number(ratioVal);
			const sub = 'str ' + ((Number.isFinite(r) && r > 0) ? (fmt(r, 2) + 'x') : '—');
			return '<div class="metric">' +
				'<div class="metric-label metric-label-row"><span>OB VPOC</span>' + regimeLegendHtml() + '</div>' +
				'<div class="metric-value ' + regimeClass(regime || 'normal') + '">' + val + '</div>' +
				'<div class="metric-value compact neutral near-volume-avg-line">' + sub + '</div>' +
			'</div>';
		}

		function volumeImbalanceBox(imbPct, velPct) {
			return '<div class="metric">' +
				'<div class="metric-label">Volume Imbalance</div>' +
				'<div class="metric-value ' + cls(imbPct) + '">' + pct(imbPct) + '</div>' +
				'<div class="metric-value compact ' + cls(velPct) + ' near-volume-avg-line">top ' + fmt(velPct, 2) + '</div>' +
				'</div>';
		}

		function sideDeltaBox(label, obDelta, nearDelta) {
			return '<div class="metric">' +
				'<div class="metric-label">' + label + '</div>' +
				'<div class="metric-value ' + cls(obDelta) + '">' + fmtDeltaPerSec(obDelta) + '/s</div>' +
				'<div class="metric-value compact ' + cls(nearDelta) + ' near-volume-avg-line">near ' + fmtDeltaPerSec(nearDelta) + '/s</div>' +
				'</div>';
		}

		function tradesCountWindowBox(delta, imbPct) {
			return '<div class="metric">' +
				'<div class="metric-label">Trades Delta ' + TRADES_DELTA_WINDOW_SEC + 's</div>' +
				'<div class="metric-value ' + cls(delta) + '">' + fmt(delta, 0) + '</div>' +
				'<div class="metric-value compact ' + cls(imbPct) + ' near-volume-avg-line">imb ' + pct(imbPct) + '</div>' +
				'</div>';
		}

		function tradesFlowImbBox(flow, deltaVolume) {
			return '<div class="metric">' +
				'<div class="metric-label">Trades Flow ' + TRADES_DELTA_WINDOW_SEC + 's</div>' +
				'<div class="metric-value ' + cls(flow) + '">' + fmt(flow, 2) + '/s</div>' +
				'<div class="metric-value compact ' + cls(deltaVolume) + ' near-volume-avg-line">vol $' + fmt(deltaVolume, 2) + '</div>' +
				'</div>';
		}

		function metricsHtml(row) {
			const digits = priceDigits(row);
			const orderbookDigits = Math.max(0, digits - 1);
			return '<div class="pair-title">' +
				'<div class="symbol">' + row.symbol + '</div>' +
			'</div>' +
			'<div class="metric-grid">' +
				compactMetric('OB Depth ' + ORDERBOOK_DEPTH_PCT + '%', fmtPrice(row.ob_min_price, orderbookDigits) + ' / ' + fmtPrice(row.ob_max_price, orderbookDigits)) +
				vpocBox(row.vpoc, row.vpoc_ratio, row.vpoc_regime, digits) +
				metric('Trades / min', row.trades_per_minute) +
				volumeImbalanceBox(row.volume_imbalance_pct, row.top_volume_velocity) +
				sideDeltaBox('BIDS DELTA', row.bids_delta_velocity, row.near_bids_delta_velocity) +
				sideDeltaBox('ASKS DELTA', row.asks_delta_velocity, row.near_asks_delta_velocity) +
				nearVolumeBox('Near Bids Vol', row.near_bids_volume_str, row.near_bids_volume_avg, row.near_bids_volume_regime) +
				nearVolumeBox('Near Asks Vol', row.near_asks_volume_str, row.near_asks_volume_avg, row.near_asks_volume_regime) +
				spreadBox('Spread', row.spread, row.spread_avg, row.spread_regime) +
				tradesCountWindowBox(row.trades_delta, row.trades_imbalance_pct) +
				tradesFlowImbBox(row.trades_flow_sec, row.trades_delta_volume) +
				metric('Slippage Avg', pct(row.slippage_avg), 'neutral') +
				regimeMetric('Volatility ' + VOLATILITY_WINDOW_SEC + 's', pct(row.volatility_pct), row.volatility_regime) +
				metric('Open Interest', row.open_interest) +
				metric('Funding', pct(row.funding_rate, 6), cls(row.funding_rate)) +
				smaBox('SMA20', row.m1_sma20, row.m1_sma20_slope, row.m1_sma20_slope_regime, row.m1_sma20_distance, digits) +
				smaBox('SMA200', row.m1_sma200, row.m1_sma200_slope, row.m1_sma200_slope_regime, row.m1_sma200_distance, digits) +
			'</div>';
		}

		function renderSections(rows) {
			const keys = rows.map(rowKey);
			if (keys.length === renderedKeys.length && keys.every((key, i) => key === renderedKeys[i])) {
				return;
			}

			Object.values(charts).forEach(chart => chart.destroy());
			charts = {};
			renderedKeys = keys;

			const grid = document.getElementById('pair-grid');
			applyChartRowSizing(rows.length);
			adaptMetricSizing(rows.length);
			grid.innerHTML = rows.map((row, i) =>
				'<section class="pair-section">' +
					'<div class="metrics-panel" id="metrics-' + i + '"></div>' +
					'<div class="chart-panel">' +
						'<div class="chart-wrap">' +
							'<div class="chart-points" id="chart-points-' + i + '">0 points</div>' +
							'<div class="depth-overlay" id="depth-' + i + '"><div class="depth-midline"></div></div>' +
							'<canvas id="chart-' + i + '"></canvas>' +
						'</div>' +
					'</div>' +
				'</section>'
			).join('');

			if (typeof Chart === 'undefined') {
				document.getElementById('meta').textContent = 'Chart.js failed to load, showing metrics only';
				return;
			}

			rows.forEach((row, i) => {
				charts[rowKey(row)] = new Chart(document.getElementById('chart-' + i), chartOptions());
			});
		}

		function updateSection(row, i) {
			const metrics = document.getElementById('metrics-' + i);
			if (metrics) metrics.innerHTML = metricsHtml(row);

			const points = document.getElementById('chart-points-' + i);
			let bids = (row.bid_prices || []).slice().reverse();
			let asks = (row.ask_prices || []).slice().reverse();
			let marks = (row.mark_prices || []).slice().reverse();
			let m1sma20s = (row.m1_sma20_prices || []).slice().reverse();
			let m1sma200s = (row.m1_sma200_prices || []).slice().reverse();
			let trades = (row.trades || []).slice().reverse();
			const trimmed = trimRollingChartWindow(bids, asks, marks, m1sma20s, m1sma200s, trades);
			bids = trimmed.bids;
			asks = trimmed.asks;
			marks = trimmed.marks;
			m1sma20s = trimmed.m1sma20s;
			m1sma200s = trimmed.m1sma200s;
			trades = trimmed.trades;
			if (points) points.textContent = bids.length + ' points / ' + chartWindowLabel();

			const chart = charts[rowKey(row)];
			if (!chart) return;
			chart.priceDecimals = priceDigits(row);
			chart.data.labels = bids.map(price => new Date(price.Time).toLocaleTimeString());
			chart.data.datasets[0].data = bids.map(price => price.Price);
			chart.data.datasets[0].label = 'bid';
			chart.data.datasets[1].data = asks.map(price => price.Price);
			chart.data.datasets[1].label = 'ask';
			chart.data.datasets[2].data = alignSeriesToPrices(bids, marks);
			chart.data.datasets[2].label = 'mark';
			chart.data.datasets[3].data = alignSeriesToPrices(bids, m1sma20s);
			chart.data.datasets[3].label = 'SMA20';
			chart.data.datasets[4].data = alignSeriesToPrices(bids, m1sma200s);
			chart.data.datasets[4].label = 'SMA200';
			chart.data.datasets[5].data = bids.map(() => row.vpoc > 0 ? row.vpoc : null);
			chart.data.datasets[5].label = 'VPOC';
			const tradeAlign = alignTradesToPrices(bids, asks, trades);
			chart.data.datasets[6].data = tradeAlign.prices;
			chart.data.datasets[6].tradeSizes = tradeAlign.sizes;
			chart.data.datasets[6].tradeStarts = tradeAlign.starts;
			chart.data.datasets[6].tradeEnds = tradeAlign.ends;
			chart.data.datasets[6].tradeSides = tradeAlign.sides;
			const tradeRadii = radiiFromTradeSizes(tradeAlign.prices, tradeAlign.sizes);
			chart.data.datasets[6].pointRadius = tradeRadii;
			chart.data.datasets[6].pointHoverRadius = tradeRadii.map(r => (r > 0 ? r + 2.5 : 0));
			chart.data.datasets[6].label = 'trades';
			setPriceScale(chart, bids, asks, marks, tradeAlign.prices, row.ob_bid_levels || [], row.ob_ask_levels || []);
			chart.update('none');
			renderOrderbookDepth(i, chart, row.ob_bid_levels || [], row.ob_ask_levels || []);
		}

		function setPriceScale(chart, bids, asks, marks, tradePrices, bidLevels, askLevels) {
			const orderbookPrices = (bidLevels || [])
				.concat(askLevels || [])
				.map(level => level.Price)
				.filter(price => price > 0);
			const prices = bids.concat(asks, marks)
				.map(price => price.Price)
				.concat((tradePrices || []).filter(price => Number.isFinite(price)))
				.filter(price => price > 0);

			if (prices.length === 0) {
				delete chart.options.scales.y.min;
				delete chart.options.scales.y.max;
				return;
			}

			const priceMin = Math.min(...prices);
			const priceMax = Math.max(...prices);
			const ratio = Math.max(0, Math.min(1, CHART_SCALE_RATIO));
			let min = priceMin;
			let max = priceMax;
			if (ratio > 0 && orderbookPrices.length > 0) {
				const obMin = Math.min(...orderbookPrices);
				const obMax = Math.max(...orderbookPrices);
				min = priceMin - Math.max(0, priceMin - obMin) * ratio;
				max = priceMax + Math.max(0, obMax - priceMax) * ratio;
			}
			const padding = ratio >= 1 && orderbookPrices.length > 0 ? 0 : Math.max((max - min) * 0.08, max * 0.0001);
			chart.options.scales.y.min = min - padding;
			chart.options.scales.y.max = max + padding;
		}

		function measuredChartHeightPx(chart) {
			const chartArea = chart?.chartArea;
			if (
				chartArea &&
				Number.isFinite(chartArea.top) &&
				Number.isFinite(chartArea.bottom) &&
				chartArea.bottom > chartArea.top
			) {
				return chartArea.bottom - chartArea.top;
			}
			return chart?.canvas?.clientHeight || chart?.height || 0;
		}

		function volumeNodeHeightPx(chart) {
			const chartHeight = measuredChartHeightPx(chart);
			const canvasHeight = chart?.canvas?.clientHeight || chart?.height || chartHeight;
			if (!(chartHeight > 0) || !(canvasHeight > 0)) {
				return 10;
			}

			const fullSizeChartHeight = chartHeight * (chartRowHeightPx(1) / canvasHeight);
			if (!(fullSizeChartHeight > 0)) {
				return 10;
			}

			return Math.round(clamp(2, 10 * (chartHeight / fullSizeChartHeight), 10));
		}

		function renderOrderbookDepth(index, chart, bidLevels, askLevels) {
			const overlay = document.getElementById('depth-' + index);
			if (!overlay || !chart || !chart.scales || !chart.scales.y) {
				return;
			}
			overlay.innerHTML = '<div class="depth-midline"></div>';

			const chartArea = chart.chartArea;
			if (!chartArea) {
				return;
			}
			const yScale = chart.scales.y;
			const bidsTop = bidLevels || [];
			const asksTop = askLevels || [];
			const bidSteps = [];
			for (const level of bidsTop) {
				const notional = (level?.Price || 0) * (level?.Size || 0);
				if (!(notional > 0) || !(level?.Price > 0)) {
					continue;
				}
				const y = yScale.getPixelForValue(level.Price);
				if (!Number.isFinite(y) || y < chartArea.top || y > chartArea.bottom) {
					continue;
				}
				bidSteps.push({ y, notional });
			}
			const askSteps = [];
			for (const level of asksTop) {
				const notional = (level?.Price || 0) * (level?.Size || 0);
				if (!(notional > 0) || !(level?.Price > 0)) {
					continue;
				}
				const y = yScale.getPixelForValue(level.Price);
				if (!Number.isFinite(y) || y < chartArea.top || y > chartArea.bottom) {
					continue;
				}
				askSteps.push({ y, notional });
			}

			drawNearVolumeMarkers(overlay, yScale, chartArea, bidsTop, asksTop);

			const totalNotional = bidSteps
				.concat(askSteps)
				.reduce((total, step) => total + step.notional, 0);
			if (!(totalNotional > 0)) {
				return;
			}

			const nodeHeight = volumeNodeHeightPx(chart);
			const draw = (steps, cls) => {
				let cumulative = 0;
				for (let i = 0; i < steps.length; i++) {
					const step = steps[i];
					cumulative += step.notional;
					const widthPct = Math.min(95, Math.max(2, (cumulative / totalNotional) * 95));
					const el = document.createElement('div');
					el.className = 'depth-level ' + cls;
					el.style.width = widthPct + '%';
					el.style.height = nodeHeight + 'px';
					const top = Math.min(chartArea.bottom - nodeHeight, Math.max(chartArea.top, step.y - (nodeHeight / 2)));
					el.style.top = Math.round(top) + 'px';
					overlay.appendChild(el);
				}
			};

			draw(bidSteps, 'depth-bid');
			draw(askSteps, 'depth-ask');
		}

		function drawNearVolumeMarkers(overlay, yScale, chartArea, bidLevels, askLevels) {
			const bestBid = bidLevels?.[0]?.Price || 0;
			const bestAsk = askLevels?.[0]?.Price || 0;
			const depthRatio = ORDERBOOK_NEAR_DEPTH_PCT / 100;
			if (!(bestBid > 0) || !(bestAsk > 0) || !(depthRatio > 0)) {
				return;
			}

			const markers = [
				{ price: bestBid, cls: 'near-marker-bid' },
				{ price: bestBid * (1 - depthRatio), cls: 'near-marker-bid near-marker-boundary' },
				{ price: bestAsk, cls: 'near-marker-ask' },
				{ price: bestAsk * (1 + depthRatio), cls: 'near-marker-ask near-marker-boundary' },
			];

			for (const marker of markers) {
				const y = yScale.getPixelForValue(marker.price);
				if (!Number.isFinite(y) || y < chartArea.top || y > chartArea.bottom) {
					continue;
				}
				const el = document.createElement('div');
				el.className = 'near-marker ' + marker.cls;
				el.style.top = Math.round(y) + 'px';
				overlay.appendChild(el);
			}
		}

		function alignSeriesToPrices(prices, series) {
			const aligned = new Array(prices.length).fill(null);
			let seriesIndex = 0;
			let latest = null;
			for (let priceIndex = 0; priceIndex < prices.length; priceIndex++) {
				const priceTime = new Date(prices[priceIndex].Time).getTime();
				while (
					seriesIndex < series.length &&
					new Date(series[seriesIndex].Time).getTime() <= priceTime
				) {
					latest = series[seriesIndex].Price;
					seriesIndex++;
				}
				aligned[priceIndex] = latest;
			}
			return aligned;
		}

		function alignTradesToPrices(bidPrices, askPrices, trades) {
			const aligned = new Array(bidPrices.length).fill(null);
			const sizes = new Array(bidPrices.length).fill(null);
			const starts = new Array(bidPrices.length).fill(null);
			const ends = new Array(bidPrices.length).fill(null);
			const sides = new Array(bidPrices.length).fill(null);
			let priceIndex = 0;
			for (const trade of trades) {
				const tradeTime = new Date(trade.Time).getTime();
				while (
					priceIndex < bidPrices.length - 1 &&
					new Date(bidPrices[priceIndex + 1].Time).getTime() <= tradeTime
				) {
					priceIndex++;
				}
				if (bidPrices.length > 0) {
					const end = trade.EndPrice || trade.Price;
					let start = trade.StartPrice || end;
					const side = trade.Side || '';
					if (start === end) {
						if (side === 'buy') {
							start = askPrices[priceIndex]?.Price || start;
						} else if (side === 'sell') {
							start = bidPrices[priceIndex]?.Price || start;
						}
					}
					aligned[priceIndex] = end;
					sizes[priceIndex] = trade.Size;
					starts[priceIndex] = start;
					ends[priceIndex] = end;
					sides[priceIndex] = side;
				}
			}
			return { prices: aligned, sizes: sizes, starts: starts, ends: ends, sides: sides };
		}

		function radiiFromTradeSizes(priceData, sizeData) {
			const radii = new Array(priceData.length).fill(0);
			let minS = Infinity;
			let maxS = -Infinity;
			for (let i = 0; i < priceData.length; i++) {
				const p = priceData[i];
				if (p == null || !Number.isFinite(p)) {
					continue;
				}
				const s = sizeData[i];
				if (s != null && Number.isFinite(s) && s > 0) {
					if (s < minS) {
						minS = s;
					}
					if (s > maxS) {
						maxS = s;
					}
				}
			}
			const rMin = 2.8;
			const rMax = 8.5;
			const defaultR = 4.2;
			for (let i = 0; i < priceData.length; i++) {
				const p = priceData[i];
				if (p == null || !Number.isFinite(p)) {
					radii[i] = 0;
					continue;
				}
				const s = sizeData[i];
				if (s == null || !Number.isFinite(s) || s <= 0) {
					radii[i] = defaultR;
					continue;
				}
				if (!Number.isFinite(minS) || minS === maxS) {
					radii[i] = defaultR;
				} else {
					const t = (s - minS) / (maxS - minS);
					radii[i] = rMin + t * (rMax - rMin);
				}
			}
			return radii;
		}

		async function refresh() {
			try {
				const response = await fetch('/api/data');
				const data = await response.json();
				if (typeof data.chart_window_seconds === 'number' && data.chart_window_seconds > 0) {
					chartWindowSeconds = data.chart_window_seconds;
				}
				renderSections(data.pairs_data);
				data.pairs_data.forEach(updateSection);

				const latency = Object.entries(data.latency_ms || {})
					.map(([exchange, ms]) => exchange + ': ' + ms + 'ms')
					.join(' | ');
				document.getElementById('meta').textContent =
					'Pairs: ' + data.pairs_data.length + ' | Last update: ' + new Date(data.last_update).toLocaleTimeString() + ' | ' + latency;
			} catch (error) {
				document.getElementById('meta').textContent = 'Disconnected: ' + error;
			}
		}

		refresh();
		setInterval(refresh, DASHBOARD_REFRESH_MS);
		window.addEventListener('resize', () => {
			applyChartRowSizing(renderedKeys.length);
			adaptMetricSizing(renderedKeys.length);
			Object.values(charts).forEach(chart => chart.resize());
		});
	</script>
</body>
</html>`

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	winSec, refMs, scale := d.effectiveDashboardPrefs()
	data := dashboardTemplateData{
		ChartWindowSeconds:   winSec,
		RefreshMs:            refMs,
		ChartScaleRatio:      scale,
		NearDepthPct:         ORDERBOOK_NEAR_DEPTH_PCT,
		OrderbookDepthPct:    ORDERBOOK_DEPTH_PCT,
		TradesDeltaWindowSec: int(TRADES_DELTA_WINDOW / time.Second),
		VolatilityWindowSec:  int(VOLATILITY_WINDOW / time.Second),
	}
	if err := template.Must(template.New("dashboard").Parse(tmpl)).Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (d *Dashboard) apiHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	if err := json.NewEncoder(w).Encode(d.getDashboardData()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (d *Dashboard) getDashboardData() DashboardData {
	winSec, _, _ := d.effectiveDashboardPrefs()
	rows := make([]DashboardPairData, 0)
	latency := make(map[string]int64, 1)

	strat := d.model
	if strat == nil || strat.Exchange == nil {
		return DashboardData{
			PairsData:          rows,
			LastUpdate:         time.Now(),
			LatencyMs:          latency,
			ChartWindowSeconds: winSec,
		}
	}

	exchangeName := strat.Exchange.Name()
	latency[exchangeName] = strat.Exchange.GetLatency()

	pairs := make([]string, 0, len(strat.traders))
	for pair := range strat.traders {
		pairs = append(pairs, pair)
	}
	sort.Strings(pairs)

	for _, pair := range pairs {
		t := strat.traders[pair]
		if t == nil {
			continue
		}

		obBids, obAsks := dashboardOrderbookLevels(strat.Exchange, pair, ORDERBOOK_LEVEL)
		obMinPrice, obMaxPrice := orderbookPriceWindow(obBids, obAsks)
		pairInfo, _ := strat.Exchange.Pair(pair)
		priceDecimals := tickSizeDecimals(pairInfo.Base.TickSize)

		t.RLock()
		midPrice := t.MidPrice
		spread := 0.0
		if t.bestBid > 0 && t.bestAsk > 0 {
			midPrice = (t.bestBid + t.bestAsk) / 2
			spread = ((t.bestAsk - t.bestBid) / midPrice) * 100
		}
		prices := copyLatestPricePairs(t.Prices)
		bidPrices, askPrices := splitBidAskPricePairs(prices)
		sampleTime := latestPriceTime(bidPrices, askPrices)
		row := DashboardPairData{
			Exchange:             exchangeName,
			Symbol:               t.Pair,
			PriceDecimals:        priceDecimals,
			MarkPrice:            t.MarkPrice,
			MidPrice:             midPrice,
			SpreadAvg:            t.spreadAvg,
			Spread:               spread,
			SpreadRegime:         spreadRegime(t.spreadAvg),
			SlippageAvg:          t.slippageAvg,
			VPOC:                 t.vpoc,
			VpocRatio:            t.vpocRatio,
			VpocRegime:           vpocRegime(t.vpoc, t.vpocRatio),
			VolumeImbalancePct:   t.volumeImbalancePct,
			TopVolumeVelocity:    t.topVolumeVelocity,
			BidsDeltaVelocity:     t.bidsDeltaVelocity,
			AsksDeltaVelocity:     t.asksDeltaVelocity,
			NearBidsDeltaVelocity: t.nearBidsDeltaVelocity,
			NearAsksDeltaVelocity: t.nearAsksDeltaVelocity,
			NearBidsVolumeStr:     t.nearBidsVolumeStr,
			NearBidsVolumeAvg:    t.nearBidsVolumeAvg,
			NearBidsVolumeRegime: nearVolumeRegime(t.nearBidsVolumeStr),
			NearAsksVolumeStr:    t.nearAsksVolumeStr,
			NearAsksVolumeAvg:    t.nearAsksVolumeAvg,
			NearAsksVolumeRegime: nearVolumeRegime(t.nearAsksVolumeStr),
			VolatilityPct:        t.volatilityPct,
			VolatilityRegime:     volatilityRegime(t.volatilityPct),
			TradesPerMinute:      t.tradesPerMinute,
			TradesFlowSec:          t.tradesFlowSec,
			TradesImbalancePct:   t.tradesImbalancePct,
			TradesDelta:          t.tradesDelta,
			TradesDeltaVolume:    t.tradesDeltaVolume,
			OpenInterest:         int64(t.openInterest),
			FundingRate:          t.fundingRate,
			M1_SMA20:             t.m1_SMA20,
			M1_SMA20Slope:        t.m1_SMA20Slope,
			M1_SMA20SlopeRegime:  smaSlopeRegime(t.m1_SMA20Slope),
			M1_SMA20Distance:     t.m1_SMA20Distance,
			M1_SMA200:            t.m1_SMA200,
			M1_SMA200Slope:       t.m1_SMA200Slope,
			M1_SMA200SlopeRegime: smaSlopeRegime(t.m1_SMA200Slope),
			M1_SMA200Distance:    t.m1_SMA200Distance,
			BidPrices:            bidPrices,
			AskPrices:            askPrices,
			MarkPrices:           dashboardPricePoint(t.MarkPrice, sampleTime),
			M1SMA20Prices:        dashboardPricePoint(t.m1_SMA20, sampleTime),
			M1SMA200Prices:       dashboardPricePoint(t.m1_SMA200, sampleTime),
			OBBidLevels:          obBids,
			OBAskLevels:          obAsks,
			OBMinPrice:           obMinPrice,
			OBMaxPrice:           obMaxPrice,
			Trades:               tradePricePoints(t.Trades),
		}
		t.RUnlock()

		d.applyHistory(rowKey(row), &row)
		rows = append(rows, row)
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Exchange == rows[j].Exchange {
			return rows[i].Symbol < rows[j].Symbol
		}
		return rows[i].Exchange < rows[j].Exchange
	})

	return DashboardData{
		PairsData:          rows,
		LastUpdate:         time.Now(),
		LatencyMs:          latency,
		ChartWindowSeconds: winSec,
	}
}

func (d *Dashboard) applyHistory(key string, row *DashboardPairData) {
	d.mu.Lock()
	defer d.mu.Unlock()

	history := d.histories[key]
	if history == nil {
		history = &dashboardHistory{}
		d.histories[key] = history
	}

	cutoff := time.Now().Add(-d.chartWindowDuration())
	incomingPairs := zipBidAskAligned(row.BidPrices, row.AskPrices)
	history.PairedQuotes = trimDashboardQuotePairs(
		mergeLatestQuotePairs(history.PairedQuotes, incomingPairs),
		cutoff,
	)
	history.MarkPrices = trimDashboardPrices(mergeLatestPrices(history.MarkPrices, row.MarkPrices), cutoff)
	history.M1SMA20Prices = trimDashboardPrices(mergeLatestPrices(history.M1SMA20Prices, row.M1SMA20Prices), cutoff)
	history.M1SMA200Prices = trimDashboardPrices(mergeLatestPrices(history.M1SMA200Prices, row.M1SMA200Prices), cutoff)
	history.Trades = trimDashboardTrades(mergeLatestTrades(history.Trades, row.Trades), cutoff)

	bids, asks := splitBidAskPricePairs(history.PairedQuotes)
	row.BidPrices = copyLatestPrices(bids)
	row.AskPrices = copyLatestPrices(asks)
	row.MarkPrices = copyLatestPrices(history.MarkPrices)
	row.M1SMA20Prices = copyLatestPrices(history.M1SMA20Prices)
	row.M1SMA200Prices = copyLatestPrices(history.M1SMA200Prices)
	row.Trades = copyLatestTrades(history.Trades)
}

func dashboardOrderbookLevels(exch exchange.I, pair string, levels int) ([]exchange.Price, []exchange.Price) {
	if exch == nil || levels <= 0 {
		return nil, nil
	}
	ob, err := exch.GetOrderbook(pair)
	if err != nil || len(ob.Bids) == 0 || len(ob.Asks) == 0 {
		return nil, nil
	}

	mid := (ob.Bids[0].Price + ob.Asks[0].Price) / 2
	if mid <= 0 {
		return nil, nil
	}
	maxDistance := mid * (ORDERBOOK_DEPTH_PCT / 100)

	bids := make([]exchange.Price, 0, levels)
	for i := 0; i < levels && i < len(ob.Bids); i++ {
		level := ob.Bids[i]
		if level.Price <= 0 || level.Size <= 0 {
			continue
		}
		if math.Abs(level.Price-mid) > maxDistance {
			break
		}
		bids = append(bids, level)
	}
	asks := make([]exchange.Price, 0, levels)
	for i := 0; i < levels && i < len(ob.Asks); i++ {
		level := ob.Asks[i]
		if level.Price <= 0 || level.Size <= 0 {
			continue
		}
		if math.Abs(level.Price-mid) > maxDistance {
			break
		}
		asks = append(asks, level)
	}
	return bids, asks
}

func orderbookPriceWindow(bids, asks []exchange.Price) (float64, float64) {
	minPrice := 0.0
	maxPrice := 0.0
	update := func(price float64) {
		if price <= 0 {
			return
		}
		if minPrice == 0 || price < minPrice {
			minPrice = price
		}
		if price > maxPrice {
			maxPrice = price
		}
	}
	for _, level := range bids {
		update(level.Price)
	}
	for _, level := range asks {
		update(level.Price)
	}
	return minPrice, maxPrice
}

func rowKey(row DashboardPairData) string {
	return row.Exchange + ":" + row.Symbol
}

func tickSizeDecimals(tickSize float64) int {
	if tickSize <= 0 {
		return 6
	}

	scaled := tickSize
	for decimals := 0; decimals <= 12; decimals++ {
		if math.Abs(scaled-math.Round(scaled)) < 1e-9 {
			return decimals
		}
		scaled *= 10
	}
	return 6
}

func copyLatestPrices(prices []exchange.Price) []exchange.Price {
	return append([]exchange.Price(nil), prices...)
}

func copyLatestTrades(trades []dashboardTradePoint) []dashboardTradePoint {
	return append([]dashboardTradePoint(nil), trades...)
}

func copyLatestPricePairs(prices [][2]exchange.Price) [][2]exchange.Price {
	return append([][2]exchange.Price(nil), prices...)
}

func trimDashboardPrices(prices []exchange.Price, cutoff time.Time) []exchange.Price {
	filtered := prices[:0]
	for _, price := range prices {
		if !price.Time.IsZero() && price.Time.Before(cutoff) {
			continue
		}
		filtered = append(filtered, price)
	}
	return filtered
}

func trimDashboardTrades(trades []dashboardTradePoint, cutoff time.Time) []dashboardTradePoint {
	filtered := trades[:0]
	for _, trade := range trades {
		if !trade.Time.IsZero() && trade.Time.Before(cutoff) {
			continue
		}
		filtered = append(filtered, trade)
	}
	return filtered
}

func dashboardPricePoint(price float64, ts time.Time) []exchange.Price {
	if price <= 0 {
		return nil
	}
	if ts.IsZero() {
		ts = time.Now()
	}
	return []exchange.Price{{
		Price: price,
		Time:  ts,
	}}
}

func latestPriceTime(priceGroups ...[]exchange.Price) time.Time {
	var latest time.Time
	for _, prices := range priceGroups {
		for _, price := range prices {
			if price.Time.After(latest) {
				latest = price.Time
			}
		}
	}
	return latest
}

func splitBidAskPricePairs(prices [][2]exchange.Price) ([]exchange.Price, []exchange.Price) {
	bids := make([]exchange.Price, 0, len(prices))
	asks := make([]exchange.Price, 0, len(prices))

	for _, price := range prices {
		bid := price[0]
		ask := price[1]
		if bid.Price <= 0 || ask.Price <= 0 || ask.Price < bid.Price {
			continue
		}
		bids = append(bids, bid)
		asks = append(asks, ask)
	}

	return bids, asks
}

// zipBidAskAligned builds one row per index from parallel bid/ask slices (same snapshot order).
func zipBidAskAligned(bids, asks []exchange.Price) [][2]exchange.Price {
	n := len(bids)
	if len(asks) < n {
		n = len(asks)
	}
	if n == 0 {
		return nil
	}
	out := make([][2]exchange.Price, n)
	for i := 0; i < n; i++ {
		out[i] = [2]exchange.Price{bids[i], asks[i]}
	}
	return out
}

type quotePairKey struct {
	Time  int64
	Bid   float64
	Ask   float64
	BidSz float64
	AskSz float64
}

func mergeLatestQuotePairs(existing, incoming [][2]exchange.Price) [][2]exchange.Price {
	seen := make(map[quotePairKey]struct{}, len(existing)+len(incoming))
	merged := make([][2]exchange.Price, 0, len(existing)+len(incoming))

	add := func(pair [2]exchange.Price) {
		bid, ask := pair[0], pair[1]
		if bid.Price <= 0 || ask.Price <= 0 || ask.Price < bid.Price {
			return
		}
		key := quotePairKey{
			Time:  bid.Time.UnixNano(),
			Bid:   bid.Price,
			Ask:   ask.Price,
			BidSz: bid.Size,
			AskSz: ask.Size,
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		merged = append(merged, pair)
	}

	for _, p := range incoming {
		add(p)
	}
	for _, p := range existing {
		add(p)
	}

	sort.SliceStable(merged, func(i, j int) bool {
		return merged[i][0].Time.After(merged[j][0].Time)
	})

	return merged
}

func trimDashboardQuotePairs(pairs [][2]exchange.Price, cutoff time.Time) [][2]exchange.Price {
	filtered := pairs[:0]
	for _, pair := range pairs {
		if !pair[0].Time.IsZero() && pair[0].Time.Before(cutoff) {
			continue
		}
		filtered = append(filtered, pair)
	}
	return filtered
}

type priceKey struct {
	Time  int64
	Price float64
	Size  float64
}

func mergeLatestPrices(existing, incoming []exchange.Price) []exchange.Price {
	seen := make(map[priceKey]struct{}, len(existing)+len(incoming))
	merged := make([]exchange.Price, 0, len(existing)+len(incoming))

	add := func(price exchange.Price) {
		if price.Price <= 0 {
			return
		}

		key := priceKey{
			Time:  price.Time.UnixNano(),
			Price: price.Price,
			Size:  price.Size,
		}
		if _, ok := seen[key]; ok {
			return
		}

		seen[key] = struct{}{}
		merged = append(merged, price)
	}

	for _, price := range incoming {
		add(price)
	}
	for _, price := range existing {
		add(price)
	}

	sort.SliceStable(merged, func(i, j int) bool {
		return merged[i].Time.After(merged[j].Time)
	})

	return merged
}

type tradePointKey struct {
	Time       int64
	Price      float64
	StartPrice float64
	EndPrice   float64
	Size       float64
	Side       string
}

func mergeLatestTrades(existing, incoming []dashboardTradePoint) []dashboardTradePoint {
	seen := make(map[tradePointKey]struct{}, len(existing)+len(incoming))
	merged := make([]dashboardTradePoint, 0, len(existing)+len(incoming))

	add := func(trade dashboardTradePoint) {
		if trade.Price <= 0 {
			return
		}

		key := tradePointKey{
			Time:       trade.Time.UnixNano(),
			Price:      trade.Price,
			StartPrice: trade.StartPrice,
			EndPrice:   trade.EndPrice,
			Size:       trade.Size,
			Side:       trade.Side,
		}
		if _, ok := seen[key]; ok {
			return
		}

		seen[key] = struct{}{}
		merged = append(merged, trade)
	}

	for _, trade := range incoming {
		add(trade)
	}
	for _, trade := range existing {
		add(trade)
	}

	sort.SliceStable(merged, func(i, j int) bool {
		return merged[i].Time.After(merged[j].Time)
	})

	return merged
}

func tradePricePoints(trades []exchange.Trade) []dashboardTradePoint {
	points := make([]dashboardTradePoint, 0, len(trades))
	for _, trade := range trades {
		point, ok := tradePricePoint(trade)
		if ok {
			points = append(points, point)
		}
	}
	return points
}

func tradePricePoint(trade exchange.Trade) (dashboardTradePoint, bool) {
	var startPrice float64
	var endPrice float64
	var size float64
	var ts time.Time
	var side string

	if trade.Order != nil {
		startPrice = trade.Order.Price
		size = trade.Order.Size
		ts = trade.Order.Time
		side = strings.ToLower(trade.Order.Side)
	}
	if side != "buy" && side != "sell" {
		for _, fill := range trade.Fills {
			if fill != nil {
				side = strings.ToLower(fill.Side)
				if side == "buy" || side == "sell" {
					break
				}
			}
		}
	}
	if side != "buy" && side != "sell" {
		return dashboardTradePoint{}, false
	}

	if len(trade.Fills) > 0 {
		var totalSize float64
		var firstFillPrice float64
		for _, fill := range trade.Fills {
			if fill == nil || fill.Price <= 0 || fill.Size <= 0 {
				continue
			}
			if ts.IsZero() {
				ts = fill.Time
			}
			if firstFillPrice == 0 {
				firstFillPrice = fill.Price
			}
			totalSize += fill.Size
			switch side {
			case "buy":
				if fill.Price > endPrice {
					endPrice = fill.Price
				}
			case "sell":
				if endPrice == 0 || fill.Price < endPrice {
					endPrice = fill.Price
				}
			}
		}
		if totalSize > 0 {
			if startPrice <= 0 {
				startPrice = firstFillPrice
			}
			size = totalSize
		}
	}

	if startPrice <= 0 {
		startPrice = endPrice
	}
	if endPrice <= 0 {
		return dashboardTradePoint{}, false
	}
	if ts.IsZero() {
		ts = time.Now()
	}

	return dashboardTradePoint{
		Price:      endPrice,
		StartPrice: startPrice,
		EndPrice:   endPrice,
		Size:       size,
		Time:       ts,
		Side:       side,
	}, true
}
