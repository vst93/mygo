//go:build darwin

package video

import (
	"bytes"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/egoist/mygo/internal/media"
	"github.com/egoist/mygo/internal/wait"
	"github.com/egoist/mygo/ui"
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

// TestFastStartWaitsForMetadata holds the HTTP source while the engine polls
// more often than the old retry budget allowed. Loading is not evidence that
// a finite video cannot be retimed. Starting at an audio handoff's position,
// duplicate speed delivery, pause/resume and a speed change while loading
// must all preserve the user's latest intent.
func TestFastStartWaitsForMetadata(t *testing.T) {
	for _, action := range []string{"play", "pause", "cancel fast rate"} {
		t.Run(action, func(t *testing.T) {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			gate, requested := make(chan struct{}), make(chan struct{})
			var releaseOnce, requestOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(gate) }) }
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestOnce.Do(func() { close(requested) })
				select {
				case <-gate:
				case <-r.Context().Done():
					return
				}
				w.Header().Set("Content-Type", "video/mp4")
				http.ServeContent(w, r, "rgb.mp4", time.Time{}, bytes.NewReader(rgbMP4))
			}))
			var p *ui.VideoPlayer
			t.Cleanup(func() {
				release()
				if p != nil {
					p.Close()
				}
				server.Close()
			})
			engine, err := newEngine(source{url: server.URL + "/rgb.mp4"})
			if errors.Is(err, ErrUnsupported) {
				t.Skip(err)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer pooled()()
			e := engine.(*avEngine)
			p = ui.NewEngineVideoPlayer(e).SetMuted(true).SetLoop(true)
			p.SetRate(3)
			tt := ui.NewTester(func(c *ui.Context) { ui.Video(c, p).Size(64, 48) }, 64, 48)
			// Window creation and the shared settings deliver this twice.
			p.SetRate(3)
			p.Seek(900 * time.Millisecond)
			wait.For(t, "the blocked HTTP request", requested)
			e.mu.Lock()
			for range 100 {
				e.poll()
			}
			pending, noRetime, state := e.pending, e.noRetime, e.state
			e.mu.Unlock()
			if pending != 1.0/3 || noRetime || state != media.Loading {
				t.Fatalf("still loading: pending %v, disabled %v, state %v", pending, noRetime, state)
			}
			if action == "pause" {
				p.Pause()
			}
			wantRate := 3.0
			if action == "cancel fast rate" {
				p.SetRate(1.5)
				wantRate = 1.5
			}
			release()
			wait.Until(t, "the first picture after loading", func() bool {
				tt.Frame()
				checkErr(t, p)
				e.mu.Lock()
				retimed := e.pending == 0
				e.mu.Unlock()
				return p.Ready() && retimed
			})
			e.mu.Lock()
			pending, factor, rate := e.pending, e.factor, e.rate
			e.mu.Unlock()
			if pending != 0 || math.Abs(float64(rate)/factor-wantRate) > 1e-6 {
				t.Fatalf("loaded at the wrong speed: pending %v, factor %v, rate %v", pending, factor, rate)
			}
			if action == "pause" {
				if p.Playing() || msgFloat32(e.player, sel("rate")) != 0 {
					t.Fatal("loading completion resumed a paused video")
				}
				if pos := p.Position(); pos < 400*time.Millisecond || pos > 1200*time.Millisecond {
					t.Fatalf("lost the audio handoff's seek: %v", pos)
				}
			} else {
				wait.Until(t, "automatic playback at the requested speed", func() bool {
					tt.Frame()
					checkErr(t, p)
					return math.Abs(float64(msgFloat32(e.player, sel("rate")))/factor-wantRate) < 1e-6
				})
			}
			p.Pause()
			p.Play()
			wait.Until(t, "playback at the requested speed after pause/resume", func() bool {
				tt.Frame()
				checkErr(t, p)
				return math.Abs(float64(msgFloat32(e.player, sel("rate")))/factor-wantRate) < 1e-6
			})
			before := tt.Image().RGBAAt(32, 24)
			wait.Until(t, "a new picture while playing", func() bool {
				tt.Frame()
				checkErr(t, p)
				return tt.Image().RGBAAt(32, 24) != before
			})
		})
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
