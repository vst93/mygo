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
