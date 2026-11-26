package cex

import (
	"context"
	"errors"
	"testing"
	"time"
)

// F is the mid of a valid quote and fresh for fx_max_age after the read. A
// failed read and an invalid quote publish nothing: they neither replace the
// value nor extend its life, and nothing stands in for a stale F.
func TestFXFreshnessAndInvalidQuotes(t *testing.T) {
	start := t0
	r := newRig(t, testConfig(), start) // fx_max_age 60 s
	ctx := context.Background()
	if _, fresh := r.m.FX(start); fresh {
		t.Fatal("F is fresh before any read")
	}

	changed := r.m.ReferenceChanged()
	r.fx.set("0.9994", "0.9996", nil)
	r.m.pollFX(ctx)
	select {
	case <-changed:
	default:
		t.Fatal("ReferenceChanged was not closed by the publication of F")
	}
	fx, fresh := r.m.FX(start)
	if !fresh || !fx.Price.Equal(dec("0.9995")) || !fx.Bid.Equal(dec("0.9994")) || !fx.Ask.Equal(dec("0.9996")) || !fx.ReceivedAt.Equal(start) {
		t.Fatalf("F = %+v fresh %t, want 0.9995 read at the start, fresh", fx, fresh)
	}

	// Thirty seconds of reads that fail or carry an invalid quote: the bid
	// above the ask, a zero side, a negative side.
	for i, bad := range []struct {
		bid, ask string
		err      error
	}{{"0", "0", errors.New("http 503")}, {"0.9997", "0.9996", nil}, {"0", "0.9996", nil}, {"0.9994", "0", nil}, {"-0.9994", "0.9996", nil}} {
		r.clock.Set(start.Add(time.Duration(5*(i+1)) * time.Second))
		r.fx.set(bad.bid, bad.ask, bad.err)
		changed = r.m.ReferenceChanged()
		r.m.pollFX(ctx)
		select {
		case <-changed:
			t.Fatalf("the read %+v published something", bad)
		default:
		}
	}
	if fx, _ := r.m.FX(r.clock.Now()); !fx.Price.Equal(dec("0.9995")) || !fx.ReceivedAt.Equal(start) {
		t.Fatalf("after invalid reads F = %+v, want the first value with its own arrival time", fx)
	}

	// Fresh up to fx_max_age after the last valid read, stale after, though
	// reads went on in between.
	if _, fresh := r.m.FX(start.Add(60 * time.Second)); !fresh {
		t.Error("F is stale exactly at fx_max_age")
	}
	fx, fresh = r.m.FX(start.Add(60*time.Second + time.Millisecond))
	if fresh || !fx.Price.Equal(dec("0.9995")) {
		t.Errorf("past fx_max_age: fresh %t price %s, want stale with the last value kept", fresh, fx.Price)
	}

	// A valid read makes it fresh again.
	r.clock.Set(start.Add(90 * time.Second))
	r.fx.set("0.9996", "0.9998", nil)
	r.m.pollFX(ctx)
	if fx, fresh := r.m.FX(start.Add(100 * time.Second)); !fresh || !fx.Price.Equal(dec("0.9997")) || !fx.ReceivedAt.Equal(start.Add(90*time.Second)) {
		t.Errorf("after a valid read F = %+v fresh %t", fx, fresh)
	}
}

// FXMinuteCloses hands a range to the source.
func TestFXMinuteCloses(t *testing.T) {
	r := newRig(t, testConfig(), t0)
	r.fx.candles[t0.UnixMilli()] = dec("0.9991")
	r.fx.candles[t0.Add(2*time.Minute).UnixMilli()] = dec("0.9993")
	got, err := r.m.FXMinuteCloses(context.Background(), t0, t0.Add(3*time.Minute))
	if err != nil || len(got) != 2 || !got[0].Minute.Equal(t0) || !got[1].Close.Equal(dec("0.9993")) {
		t.Errorf("FXMinuteCloses = %v, %v", got, err)
	}
}
