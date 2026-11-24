//go:build windows && (amd64 || arm64)

package video

import (
	"errors"
	"runtime"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/egoist/mygo/internal/media"
	"github.com/egoist/mygo/internal/scene"
)

// mfFrameStub supplies only the COM methods Frame uses. It lets the test
// omit PLAYING/CANPLAY entirely, and control whether a new picture really
// reaches the shared texture, without a decoder or a display driver.
type mfFrameStub struct {
	vtbl                    *[45]uintptr
	tick, acquire, transfer uintptr
	w, h                    uint32
}

// Callbacks are made once: Windows cannot free a syscall callback.
var mfFrameVtbl = func() [45]uintptr {
	var v [45]uintptr
	v[engineOnVideoStreamTick] = syscall.NewCallback(func(obj, pts uintptr) uintptr {
		*(*int64)(cptr(pts)) = 0
		return (*mfFrameStub)(cptr(obj)).tick
	})
	v[engineGetNativeVideoSize] = syscall.NewCallback(func(obj, w, h uintptr) uintptr {
		s := (*mfFrameStub)(cptr(obj))
		*(*uint32)(cptr(w)), *(*uint32)(cptr(h)) = s.w, s.h
		return 0
	})
	v[engineTransferVideoFrame] = syscall.NewCallback(func(obj, tex, src, dst, border uintptr) uintptr {
		return (*mfFrameStub)(cptr(obj)).transfer
	})
	v[keyedAcquireSync] = syscall.NewCallback(func(obj, key, timeout uintptr) uintptr {
		return (*mfFrameStub)(cptr(obj)).acquire
	})
	v[keyedReleaseSync] = syscall.NewCallback(func(obj, key uintptr) uintptr { return 0 })
	v[engineIsSeeking] = syscall.NewCallback(func(obj uintptr) uintptr { return 0 })
	return v
}()

func stubMFFrame() (*mfEngine, *mfFrameStub) {
	s := &mfFrameStub{vtbl: &mfFrameVtbl, w: 4, h: 4}
	obj := uintptr(unsafe.Pointer(s))
	e := &mfEngine{engine: obj, shown: 0, state: media.Ready}
	for i := range e.ring {
		e.ring[i] = sharedFrame{tex: 1, mutex: obj, w: 4, h: 4, surface: &scene.Surface{}}
	}
	return e, s
}

func TestMFSeekBufferingRecoversByFrame(t *testing.T) {
	e, s := stubMFFrame()
	defer runtime.KeepAlive(s)
	e.event(eventWaiting, 0, 0)
	e.seekSince = time.Now()
	e.event(eventSeeked, 0, 0)
	if state, _ := e.State(); state != media.Buffering {
		t.Fatal("SEEKED alone cleared buffering")
	}
	for _, step := range []struct {
		name                    string
		tick, acquire, transfer uintptr
		width                   uint32
		want                    media.State
		changed                 bool
	}{
		{"cached picture", 1, 0, 0, 4, media.Buffering, false}, // S_FALSE
		{"no video size", 0, 0, 0, 0, media.Buffering, false},
		{"texture busy", 0, 0x102, 0, 4, media.Buffering, false}, // WAIT_TIMEOUT
		{"transfer failed", 0, 0, 0x80004005, 4, media.Buffering, false},
		{"new picture", 0, 0, 0, 4, media.Ready, true},
	} {
		t.Run(step.name, func(t *testing.T) {
			s.tick, s.acquire, s.transfer, s.w = step.tick, step.acquire, step.transfer, step.width
			_, changed, ok := e.Frame(time.Time{})
			state, err := e.State()
			if !ok || changed != step.changed || state != step.want || err != nil {
				t.Fatalf("picture %v, changed %v, state %v, err %v; want changed %v, state %v", ok, changed, state, err, step.changed, step.want)
			}
		})
	}
	// Recovery must not suppress a later, genuine stall.
	e.event(eventWaiting, 0, 0)
	s.tick = 1
	e.Frame(time.Time{})
	if state, _ := e.State(); state != media.Buffering {
		t.Fatal("cached recovered picture hid a new stall")
	}
}

func TestMFFramePreservesTerminalState(t *testing.T) {
	for _, state := range []media.State{media.Loading, media.Ready, media.Ended, media.Failed} {
		e, s := stubMFFrame()
		e.state = state
		if state == media.Failed {
			e.err = errors.New("decoder failed")
		}
		wantErr := e.err
		_, changed, ok := e.Frame(time.Time{})
		got, err := e.State()
		want := state
		if state == media.Loading {
			want = media.Ready
		}
		if !ok || !changed || got != want || err != wantErr {
			t.Fatalf("from %v: picture %v, changed %v, state %v, err %v", state, ok, changed, got, err)
		}
		runtime.KeepAlive(s)
	}
}
