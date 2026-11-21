//go:build darwin

package video

import (
	"math"
	"testing"
)

// TestTimelineFor pins the timeline squeeze: up to maxFullRate the player
// itself makes the speed, above it the timeline alone does, so that the
// player still runs where it hands the video output every picture.
func TestTimelineFor(t *testing.T) {
	for _, c := range []struct {
		speed, factor, rate float64
	}{
		{0.5, 1, 0.5},
		{1, 1, 1},
		{1.5, 1, 1.5},
		{2, 1, 2},
		{2.5, 0.4, 1},
		{3, 1.0 / 3.0, 1},
		{4, 0.25, 1},
	} {
		factor, rate := timelineFor(c.speed)
		if math.Abs(factor-c.factor) > 1e-9 || rate != c.rate {
			t.Errorf("timelineFor(%v) = %v, %v; want %v, %v", c.speed, factor, rate, c.factor, c.rate)
		}
		if got := rate / factor; math.Abs(got-c.speed) > 1e-9 {
			t.Errorf("speed %v: timeline %v at rate %v plays %v×, want %v×", c.speed, factor, rate, got, c.speed)
		}
	}
}

// TestSeekSlackScalesWithTimeline pins that a seek lands within the same slack
// on the viewer's clock whatever the speed: the tolerance is applied in the
// item's timeline, which runs 1/factor times as long. Without the scaling a
// squeezed timeline allows 1/factor times as much — 1.5 s at 3× — which is a
// visible jump on every drag.
func TestSeekSlackScalesWithTimeline(t *testing.T) {
	for _, c := range []struct {
		speed, factor float64
	}{
		{1, 1}, {1.5, 1}, {2, 1}, {2.5, 0.4}, {3, 1.0 / 3.0}, {4, 0.25},
	} {
		factor, _ := timelineFor(c.speed)
		if factor != c.factor {
			t.Fatalf("timelineFor(%v) = %v; want %v", c.speed, factor, c.factor)
		}
		// On the viewer's clock the slack is what was asked for.
		if got := float64(seekSlack(factor)) / factor; got != float64(seekTolerance) {
			t.Errorf("speed %v: slack on the asset clock is %v; want %v", c.speed, got, seekTolerance)
		}
	}
	if got := seekSlack(0); got != seekTolerance {
		t.Errorf("seekSlack(0) = %v; want %v", got, seekTolerance)
	}
}
