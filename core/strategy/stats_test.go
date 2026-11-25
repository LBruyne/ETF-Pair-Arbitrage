package strategy

import (
	"math"
	"testing"
	"time"
)

// V1: the spread formula on hand numbers.
func TestSpread(t *testing.T) {
	tests := []struct {
		name                          string
		f, pBull, aBull, pBear, aBear float64
		want                          float64
	}{
		{"both legs at their anchors", 1, 40, 40, 60, 60, 0},
		{"the appendix's mean", 1, 40.40, 40, 59.42, 60, 10000 * (0.01 - 0.58/60)},          // 100 - 96.6667 = 3.3333
		{"the appendix's entry prices", 1, 40.44, 40, 59.42, 60, 10000 * (0.011 - 0.58/60)}, // 110 - 96.6667 = 13.3333
		{"F scales both legs", 0.9995, 40.44, 40, 59.42, 60, 3.326666666666667},             // 10000 x (0.9995 x 2.00133333 - 2)
		{"one percent on each leg", 1, 40.4, 40, 60.6, 60, 200},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wantFloat(t, "D", Spread(tc.f, tc.pBull, tc.aBull, tc.pBear, tc.aBear), tc.want)
		})
	}
}

// spreadBars builds a run of minutes whose spreads under the test anchors
// (40 / 60) are the given values: F = 1, the bear leg at its anchor and the
// bull leg at 40 x (1 + spread / 10000).
func spreadBars(spreads ...float64) []MinuteBar {
	run := make([]MinuteBar, len(spreads))
	for i, s := range spreads {
		run[i] = MinuteBar{Minute: t0.Add(time.Duration(i) * time.Minute), MidBull: 40 * (1 + s/10000), MidBear: 60, F: 1, Valid: true}
	}
	return run
}

// V1: the EMA (seeded with the first minute, alpha = 2 / (X + 1)), the SMA
// and the population sigma on the minute spreads 4, 6, 5.
func TestComputeStats_HandNumbers(t *testing.T) {
	run := spreadBars(4, 6, 5)
	tests := []struct {
		name      string
		model     MeanModel
		x, y      int
		wantMu    float64
		wantSigma float64
		wantReady bool
	}{
		// The SPEC's example: 4 -> 4 + 0.5 x (6 - 4) = 5 -> 5 + 0.5 x (5 - 5) = 5;
		// sigma = sqrt(((4-5)^2 + (6-5)^2 + 0) / 3) = 0.8165.
		{"EMA 3, sigma 3", MeanEMA, 3, 3, 5, math.Sqrt(2.0 / 3), true},
		{"EMA 2: alpha 2/3", MeanEMA, 2, 3, 5.111111111111111, math.Sqrt(2.0 / 3), true}, // 4 -> 5.3333 -> 5.1111
		{"EMA 1 is the last minute", MeanEMA, 1, 3, 5, math.Sqrt(2.0 / 3), true},
		{"SMA 3", MeanSMA, 3, 3, 5, math.Sqrt(2.0 / 3), true},
		{"SMA 2: the last two minutes", MeanSMA, 2, 3, 5.5, math.Sqrt(2.0 / 3), true},
		{"sigma 2: the last two minutes", MeanSMA, 3, 2, 5, 0.5, true}, // 6 and 5 around 5.5
		{"sigma 1: one minute has no spread of its own", MeanSMA, 3, 1, 5, 0, true},
		{"SMA longer than the run: what there is, not ready", MeanSMA, 5, 3, 5, math.Sqrt(2.0 / 3), false},
		{"sigma longer than the run: not ready", MeanEMA, 3, 4, 5, math.Sqrt(2.0 / 3), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := ComputeStats(run, Config{MeanModel: tc.model, MeanMinutes: tc.x, SigmaMinutes: tc.y})
			wantFloat(t, "mu", s.Mu(testAnchors), tc.wantMu)
			wantFloat(t, "sigma", s.Sigma(testAnchors), tc.wantSigma)
			if s.Ready != tc.wantReady {
				t.Errorf("Ready = %v, want %v", s.Ready, tc.wantReady)
			}
		})
	}
}

// V1: the warm-up takes max(X, Y) consecutive valid minutes.
func TestComputeStats_Warmup(t *testing.T) {
	run := spreadBars(1, 2, 3, 4, 5, 6)
	tests := []struct {
		name          string
		x, y, minutes int
		wantReady     bool
	}{
		{"X 3, Y 5: four minutes", 3, 5, 4, false},
		{"X 3, Y 5: five minutes", 3, 5, 5, true},
		{"X 5, Y 3: four minutes", 5, 3, 4, false},
		{"X 5, Y 3: five minutes", 5, 3, 5, true},
		{"X 5, Y 3: six minutes", 5, 3, 6, true},
		{"no minutes", 1, 1, 0, false},
	}
	for _, tc := range tests {
		for _, model := range []MeanModel{MeanEMA, MeanSMA} {
			s := ComputeStats(run[:tc.minutes], Config{MeanModel: model, MeanMinutes: tc.x, SigmaMinutes: tc.y})
			if s.Ready != tc.wantReady {
				t.Errorf("%s, %s: Ready = %v, want %v", tc.name, model, s.Ready, tc.wantReady)
			}
		}
	}
	if s := ComputeStats(nil, Config{MeanModel: MeanEMA, MeanMinutes: 1, SigmaMinutes: 1}); !s.AsOf.IsZero() || s.Sigma(testAnchors) != 0 {
		t.Errorf("an empty run must give empty statistics, got %+v", s)
	}
}

// V1: only the minutes of the run enter. The caller ends the run at the last
// finished minute and starts it after an invalid minute, so neither the open
// minute nor anything before the gap is seen.
func TestComputeStats_OnlyTheRun(t *testing.T) {
	cfg := Config{MeanModel: MeanEMA, MeanMinutes: 3, SigmaMinutes: 3}

	// Minutes 4, 6, 5 are finished; the open minute would be 50.
	all := spreadBars(4, 6, 5, 50)
	s := ComputeStats(all[:3], cfg)
	wantFloat(t, "mu without the open minute", s.Mu(testAnchors), 5)
	if !s.AsOf.Equal(all[2].Minute) {
		t.Errorf("AsOf = %s, want the last finished minute %s", s.AsOf, all[2].Minute)
	}

	// Minutes 4, 6, then an invalid minute, then 5, 7: the run restarts
	// after the gap. The EMA is seeded with 5: 5 + 0.5 x (7 - 5) = 6, and
	// the warm-up starts again.
	bars := spreadBars(4, 6, 0, 5, 7, 6)
	bars[2] = MinuteBar{Minute: bars[2].Minute}
	s = ComputeStats(bars[3:5], cfg)
	wantFloat(t, "mu after the restart", s.Mu(testAnchors), 6)
	wantFloat(t, "sigma after the restart", s.Sigma(testAnchors), 1) // 5 and 7 around 6
	if s.Ready {
		t.Error("two minutes after a restart must not be ready with X = Y = 3")
	}
	s = ComputeStats(bars[3:], cfg)
	wantFloat(t, "mu, third minute after the restart", s.Mu(testAnchors), 6) // 6 + 0.5 x (6 - 6)
	if !s.Ready {
		t.Error("three minutes after a restart must be ready with X = Y = 3")
	}
}

// V1: the mean is kept per leg in USD and recombined under the anchors. That
// equals the mean of the minute spreads under the same anchors, for both
// models; and the sigma recomputed under a second pair of anchors equals the
// direct computation on the minute spreads under that pair.
func TestStats_RecombinedEqualsDirect(t *testing.T) {
	run := []MinuteBar{
		{MidBull: 40.40, MidBear: 59.42, F: 1.0000},
		{MidBull: 40.52, MidBear: 59.31, F: 0.9998},
		{MidBull: 40.47, MidBear: 59.36, F: 1.0003},
		{MidBull: 40.31, MidBear: 59.58, F: 0.9995},
		{MidBull: 40.66, MidBear: 59.02, F: 1.0001},
		{MidBull: 40.58, MidBear: 59.17, F: 1.0004},
	}
	for i := range run {
		run[i].Minute, run[i].Valid = t0.Add(time.Duration(i)*time.Minute), true
	}
	const x, y = 4, 5
	for _, a := range []struct{ bull, bear string }{{"40", "60"}, {"41.3712", "58.2231"}} {
		anchors := anchors(a.bull, a.bear)
		aBull, aBear := anchors.Bull.InexactFloat64(), anchors.Bear.InexactFloat64()
		spreads := make([]float64, len(run))
		for i, b := range run {
			spreads[i] = Spread(b.F, b.MidBull, aBull, b.MidBear, aBear)
		}

		// The mean and sigma taken directly on the minute spreads.
		ema := spreads[0]
		for _, s := range spreads[1:] {
			ema += 2.0 / (x + 1) * (s - ema)
		}
		var sma float64
		for _, s := range spreads[len(spreads)-x:] {
			sma += s / x
		}
		var mean, variance float64
		for _, s := range spreads[len(spreads)-y:] {
			mean += s / y
		}
		for _, s := range spreads[len(spreads)-y:] {
			variance += (s - mean) * (s - mean) / y
		}

		// The same Stats serves both pairs of anchors.
		emaStats := ComputeStats(run, Config{MeanModel: MeanEMA, MeanMinutes: x, SigmaMinutes: y})
		smaStats := ComputeStats(run, Config{MeanModel: MeanSMA, MeanMinutes: x, SigmaMinutes: y})
		wantFloat(t, "EMA under "+a.bull+" / "+a.bear, emaStats.Mu(anchors), ema)
		wantFloat(t, "SMA under "+a.bull+" / "+a.bear, smaStats.Mu(anchors), sma)
		wantFloat(t, "sigma under "+a.bull+" / "+a.bear, emaStats.Sigma(anchors), math.Sqrt(variance))
	}
}

// A window of equal minutes has a sigma of exactly zero, whatever the
// anchors: the entry check compares it with zero.
func TestStats_FlatWindowIsExactlyZero(t *testing.T) {
	run := make([]MinuteBar, 7)
	for i := range run {
		run[i] = MinuteBar{Minute: t0.Add(time.Duration(i) * time.Minute), MidBull: 40.37, MidBear: 59.431, F: 0.9997, Valid: true}
	}
	s := ComputeStats(run, Config{MeanModel: MeanSMA, MeanMinutes: 7, SigmaMinutes: 7})
	for _, a := range []struct{ bull, bear string }{{"40", "60"}, {"41.3712", "58.2231"}} {
		if sigma := s.Sigma(anchors(a.bull, a.bear)); sigma != 0 {
			t.Errorf("sigma under %s / %s = %g, want exactly 0", a.bull, a.bear, sigma)
		}
	}
}
