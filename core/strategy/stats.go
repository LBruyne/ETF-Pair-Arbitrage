package strategy

import (
	"math"

	"etf-pair-arbitrage/core/cex"
)

// ComputeStats computes the mean and the sigma window from run: the
// consecutive valid minutes since the last restart, oldest first. Only
// finished minutes are in a run, so the open minute never enters. The caller
// decides where a run starts (a new Session, an invalid minute, a dividend
// taking effect).
//
// The mean is taken on each leg's USD price (F x mid) and not on the minute
// spread. The spread is linear in the two prices, so recombining the two
// means under a pair of anchors gives the mean of the minute spreads under
// those anchors, for the EMA and the SMA alike.
func ComputeStats(run []MinuteBar, cfg Config) Stats {
	var s Stats
	if len(run) == 0 {
		return s
	}
	switch cfg.MeanModel {
	case MeanEMA: // seeded with the first minute of the run
		mean, alpha := legUSD(run[0]), 2/float64(cfg.MeanMinutes+1)
		for _, b := range run[1:] {
			u := legUSD(b)
			mean[0] += alpha * (u[0] - mean[0])
			mean[1] += alpha * (u[1] - mean[1])
		}
		s.MeanBullUSD, s.MeanBearUSD = mean[0], mean[1]
	case MeanSMA: // the arithmetic mean of the last X minutes
		last := lastMinutes(run, cfg.MeanMinutes)
		var sum [2]float64
		for _, b := range last {
			u := legUSD(b)
			sum[0] += u[0]
			sum[1] += u[1]
		}
		s.MeanBullUSD, s.MeanBearUSD = sum[0]/float64(len(last)), sum[1]/float64(len(last))
	}
	for _, b := range lastMinutes(run, cfg.SigmaMinutes) {
		s.Window = append(s.Window, legUSD(b))
	}
	s.Ready = len(run) >= max(cfg.MeanMinutes, cfg.SigmaMinutes)
	s.AsOf = run[len(run)-1].Minute
	return s
}

// Mu is the mean of the spread under the anchors a, in spread units.
func (s Stats) Mu(a cex.Anchors) float64 {
	return Spread(1, s.MeanBullUSD, a.Bull.InexactFloat64(), s.MeanBearUSD, a.Bear.InexactFloat64())
}

// Sigma is the population standard deviation of the window's minute spreads
// under the anchors a, in spread units. It is not linear in the prices, so
// it is computed from the window each time and not stored.
func (s Stats) Sigma(a cex.Anchors) float64 {
	n := float64(len(s.Window))
	if n == 0 {
		return 0
	}
	aBull, aBear := a.Bull.InexactFloat64(), a.Bear.InexactFloat64()
	spread := func(u [2]float64) float64 { return Spread(1, u[0], aBull, u[1], aBear) }

	// The spreads are measured from the first minute's. The standard
	// deviation does not depend on where they are measured from, and a
	// window of equal minutes then gives exactly zero.
	first := spread(s.Window[0])
	var sum float64
	for _, u := range s.Window {
		sum += spread(u) - first
	}
	mean := sum / n
	var squares float64
	for _, u := range s.Window {
		d := spread(u) - first - mean
		squares += d * d
	}
	return math.Sqrt(squares / n)
}

// legUSD is the two legs' USD prices of one minute: F x mid.
func legUSD(b MinuteBar) [2]float64 {
	return [2]float64{b.F * b.MidBull, b.F * b.MidBear}
}

// lastMinutes returns the last n minutes of run, or all of it when it is
// shorter.
func lastMinutes(run []MinuteBar, n int) []MinuteBar {
	if len(run) > n {
		return run[len(run)-n:]
	}
	return run
}
