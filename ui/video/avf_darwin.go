//go:build darwin

package video

import (
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"

	"github.com/egoist/mygo/internal/media"
	"github.com/egoist/mygo/internal/scene"
)

// On macOS, AVFoundation plays the video: an AVPlayer, which decodes in
// hardware (VideoToolbox), plays the sound and keeps the clock, and an
// AVPlayerItemVideoOutput, from which each frame the window builds takes
// the picture due as it shows, a CVPixelBuffer in NV12 backed by an
// IOSurface, which the Metal renderer wraps as textures without a copy.
// A goroutine of the engine's watches how loading and playing go, and
// wakes the window as that changes. Bytes in memory reach the player
// through an AVAssetResourceLoader, whose delegate answers on a queue of
// its own from the bytes, without a file.

var (
	avfOnce sync.Once
	avfErr  error

	msgSend, msgSendStret uintptr

	msgItemTimeForHostTime func(obj objc.ID, s objc.SEL, host float64) cmTime
	msgCMTime              func(obj objc.ID, s objc.SEL) cmTime
	msgHasNewBuffer        func(obj objc.ID, s objc.SEL, t cmTime) bool
	msgCopyBuffer          func(obj objc.ID, s objc.SEL, t cmTime, display uintptr) uintptr
	msgSeek                func(obj objc.ID, s objc.SEL, t, before, after cmTime)
	msgInsertTimeRange     func(obj objc.ID, s objc.SEL, span cmTimeRange, track objc.ID, at cmTime, err *objc.ID) bool
	msgScaleTimeRange      func(obj objc.ID, s objc.SEL, span cmTimeRange, to cmTime)
	msgSetFloat32          func(obj objc.ID, s objc.SEL, v float32)
	msgFloat32             func(obj objc.ID, s objc.SEL) float32
	msgSetBool             func(obj objc.ID, s objc.SEL, v bool)
	msgInt                 func(obj objc.ID, s objc.SEL) int
	msgBool                func(obj objc.ID, s objc.SEL) bool

	caCurrentMediaTime          func() float64
	cvBufferRelease             func(buf uintptr)
	cvBufferRetain              func(buf uintptr) uintptr
	cvBufferGetAttachment       func(buf, key, mode uintptr) uintptr
	cvPixelBufferGetWidth       func(buf uintptr) uint
	cvPixelBufferGetHeight      func(buf uintptr) uint
	cvPixelBufferGetPixelFormat func(buf uintptr) uint32
	cvPixelBufferLock           func(buf uintptr, flags uint64) int32
	cvPixelBufferUnlock         func(buf uintptr, flags uint64) int32
	cvPixelBufferPlaneBase      func(buf uintptr, plane uint) unsafe.Pointer
	cvPixelBufferPlaneStride    func(buf uintptr, plane uint) uint
	cfEqual                     func(a, b uintptr) bool
	dispatchQueueCreate         func(label string, attr uintptr) uintptr
	poolPush                    func() uintptr
	poolPop                     func(pool uintptr)

	// The constants of CoreVideo and AVFoundation the engine compares
	// and passes.
	cvMatrixKey, cvMatrix709, cvMatrix601, cvMatrix2020 uintptr
	cvPixelFormatKey, cvMetalCompatibilityKey           uintptr
	avMediaTypeVideo, avMediaTypeAudio                  uintptr
	avPitchSpectral                                     uintptr

	// loaderClass is the class of the delegates answering for bytes in
	// memory; loaders their sources, by the number in their URL.
	loaderClass objc.Class
	loaderQueue uintptr
	loaders     sync.Map // uint64 → []byte
	lastLoader  atomic.Uint64
)

// cmTime is a CMTime.
type cmTime struct {
	Value     int64
	Timescale int32
	Flags     uint32
	Epoch     int64
}

const (
	cmTimeValid      = 1
	cmTimeIndefinite = 16
	cmTimeInfinite   = 4 | 8

	pixelFormat420v = 0x34323076 // kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange
	pixelFormat420f = 0x34323066 // kCVPixelFormatType_420YpCbCr8BiPlanarFullRange

	avStatusReadyToPlay = 1
	avStatusFailed      = 2
	avWaitingToPlay     = 1 // AVPlayerTimeControlStatusWaitingToPlayAtSpecifiedRate
	avActionAtEndNone   = 2
)

func (t cmTime) duration() (time.Duration, bool) {
	if t.Flags&cmTimeValid == 0 || t.Flags&(cmTimeIndefinite|cmTimeInfinite) != 0 || t.Timescale <= 0 {
		return 0, false
	}
	return time.Duration(float64(t.Value) / float64(t.Timescale) * float64(time.Second)), true
}

func cmTimeOf(d time.Duration) cmTime {
	return cmTime{Value: int64(d), Timescale: int32(time.Second), Flags: cmTimeValid}
}

// cmTimeRange is a CMTimeRange.
type cmTimeRange struct {
	Start, Duration cmTime
}

// wholeOf is the range the whole of d spans, from zero.
func wholeOf(d time.Duration) cmTimeRange {
	return cmTimeRange{Start: cmTime{Flags: cmTimeValid, Timescale: 1}, Duration: cmTimeOf(d)}
}

var sels sync.Map

func sel(name string) objc.SEL {
	if s, ok := sels.Load(name); ok {
		return s.(objc.SEL)
	}
	s := objc.RegisterName(name)
	sels.Store(name, s)
	return s
}

func send(obj objc.ID, name string, args ...any) objc.ID { return obj.Send(sel(name), args...) }

func class(name string) objc.ID { return objc.ID(objc.GetClass(name)) }

// cptr is the pointer of an address of native memory.
func cptr(a uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&a)) }

func nsString(s string) objc.ID {
	return send(class("NSString"), "stringWithUTF8String:", s)
}

func goString(str objc.ID) string {
	if str == 0 {
		return ""
	}
	p := uintptr(send(str, "UTF8String"))
	if p == 0 {
		return ""
	}
	n := 0
	for *(*byte)(cptr(p + uintptr(n))) != 0 {
		n++
	}
	return string(unsafe.Slice((*byte)(cptr(p)), n))
}

func loadAVFoundation() error {
	avfOnce.Do(func() {
		defer func() {
			if v := recover(); v != nil {
				avfErr = errors.Join(ErrUnsupported, errors.New("video: AVFoundation: "+toString(v)))
			}
		}()
		open := func(path string) uintptr {
			h, err := purego.Dlopen(path, purego.RTLD_GLOBAL|purego.RTLD_NOW)
			if err != nil {
				panic(err)
			}
			return h
		}
		objcLib := open("/usr/lib/libobjc.A.dylib")
		open("/System/Library/Frameworks/Foundation.framework/Foundation")
		av := open("/System/Library/Frameworks/AVFoundation.framework/AVFoundation")
		cv := open("/System/Library/Frameworks/CoreVideo.framework/CoreVideo")
		cf := open("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation")
		qc := open("/System/Library/Frameworks/QuartzCore.framework/QuartzCore")
		sys := open("/usr/lib/libSystem.B.dylib")
		sym := func(lib uintptr, name string) uintptr {
			p, err := purego.Dlsym(lib, name)
			if err != nil {
				panic(err)
			}
			return p
		}
		constant := func(lib uintptr, name string) uintptr { return *(*uintptr)(cptr(sym(lib, name))) }
		msgSend = sym(objcLib, "objc_msgSend")
		msgSendStret = msgSend
		if runtime.GOARCH == "amd64" {
			msgSendStret = sym(objcLib, "objc_msgSend_stret")
		}
		purego.RegisterFunc(&msgItemTimeForHostTime, msgSendStret)
		purego.RegisterFunc(&msgCMTime, msgSendStret)
		purego.RegisterFunc(&msgHasNewBuffer, msgSend)
		purego.RegisterFunc(&msgCopyBuffer, msgSend)
		purego.RegisterFunc(&msgSeek, msgSend)
		purego.RegisterFunc(&msgInsertTimeRange, msgSend)
		purego.RegisterFunc(&msgScaleTimeRange, msgSend)
		purego.RegisterFunc(&msgSetFloat32, msgSend)
		purego.RegisterFunc(&msgFloat32, msgSend)
		purego.RegisterFunc(&msgSetBool, msgSend)
		purego.RegisterFunc(&msgInt, msgSend)
		purego.RegisterFunc(&msgBool, msgSend)
		purego.RegisterLibFunc(&caCurrentMediaTime, qc, "CACurrentMediaTime")
		purego.RegisterLibFunc(&cvBufferRelease, cv, "CVBufferRelease")
		purego.RegisterLibFunc(&cvBufferRetain, cv, "CVBufferRetain")
		purego.RegisterLibFunc(&cvBufferGetAttachment, cv, "CVBufferGetAttachment")
		purego.RegisterLibFunc(&cvPixelBufferGetWidth, cv, "CVPixelBufferGetWidth")
		purego.RegisterLibFunc(&cvPixelBufferGetHeight, cv, "CVPixelBufferGetHeight")
		purego.RegisterLibFunc(&cvPixelBufferGetPixelFormat, cv, "CVPixelBufferGetPixelFormatType")
		purego.RegisterLibFunc(&cvPixelBufferLock, cv, "CVPixelBufferLockBaseAddress")
		purego.RegisterLibFunc(&cvPixelBufferUnlock, cv, "CVPixelBufferUnlockBaseAddress")
		purego.RegisterLibFunc(&cvPixelBufferPlaneBase, cv, "CVPixelBufferGetBaseAddressOfPlane")
		purego.RegisterLibFunc(&cvPixelBufferPlaneStride, cv, "CVPixelBufferGetBytesPerRowOfPlane")
		purego.RegisterLibFunc(&cfEqual, cf, "CFEqual")
		purego.RegisterLibFunc(&dispatchQueueCreate, sys, "dispatch_queue_create")
		purego.RegisterLibFunc(&poolPush, objcLib, "objc_autoreleasePoolPush")
		purego.RegisterLibFunc(&poolPop, objcLib, "objc_autoreleasePoolPop")
		cvMatrixKey = constant(cv, "kCVImageBufferYCbCrMatrixKey")
		cvMatrix709 = constant(cv, "kCVImageBufferYCbCrMatrix_ITU_R_709_2")
		cvMatrix601 = constant(cv, "kCVImageBufferYCbCrMatrix_ITU_R_601_4")
		cvMatrix2020 = constant(cv, "kCVImageBufferYCbCrMatrix_ITU_R_2020")
		cvPixelFormatKey = constant(cv, "kCVPixelBufferPixelFormatTypeKey")
		cvMetalCompatibilityKey = constant(cv, "kCVPixelBufferMetalCompatibilityKey")
		avMediaTypeVideo = constant(av, "AVMediaTypeVideo")
		avMediaTypeAudio = constant(av, "AVMediaTypeAudio")
		avPitchSpectral = constant(av, "AVAudioTimePitchAlgorithmSpectral")
		if class("AVPlayer") == 0 || class("AVPlayerItemVideoOutput") == 0 {
			panic("no AVPlayer")
		}
	})
	return avfErr
}

// pooled pushes an autorelease pool on the thread, which the goroutine
// keeps until the function it returns pops it.
func pooled() func() {
	runtime.LockOSThread()
	p := poolPush()
	return func() {
		poolPop(p)
		runtime.UnlockOSThread()
	}
}

func toString(v any) string {
	if err, ok := v.(error); ok {
		return err.Error()
	}
	if s, ok := v.(string); ok {
		return s
	}
	return "failed to load"
}

// avEngine plays a video with an AVPlayer.
type avEngine struct {
	player, item, output objc.ID
	// asset is what the item plays, kept to make another item from it
	// when the speed changes the timeline.
	asset                objc.ID
	loader               uint64 // in loaders, for bytes in memory; 0 for none
	delegate             objc.ID
	quit                 chan struct{}
	done                 chan struct{}

	// buf is the CVPixelBuffer that Frame last returned, retained, and
	// surface the surface showing it.
	buf     uintptr
	surface *scene.Surface
	w, h    int
	// interval is Interval's, which the watching goroutine reads once
	// the item is ready to play: 0 until it has.
	interval atomic.Int64

	mu    sync.Mutex
	wake  func()
	state media.State
	err   error
	// playing is whether the app plays it, loop whether it plays again
	// at its end, which the watching goroutine sees.
	playing, loop, closed bool
	rate                  float32
	// factor is how much faster the item's timeline runs than the asset's:
	// 1 for the asset itself, 1/speed for a timeline squeezed to play above
	// maxFullRate (see retime).
	factor float64
	// pending is the factor to retime to once the asset knows its duration:
	// an app sets the speed as it opens, while the tracks still load.
	pending float64
	// tries counts how often the retime was attempted: an asset whose duration
	// never comes (a stream, a file still loading) must not be rebuilt every
	// 25 ms for the rest of the session - that is what "keeps loading" was.
	tries int
	// noRetime tells that a retime failed for good: the player keeps its own
	// timeline and plays at the fastest full-frame rate rather than trying
	// forever.
	noRetime bool
	// rebase is where playback stands while the item a retime made still
	// seeks there: the new item's currentTime reads zero until it does, and
	// a position that stepped back would step the progress bar back.
	rebase atomic.Int64
	// retired are the outputs of items a retime replaced, freed in Close:
	// Frame takes one on the thread drawing the window, which may hold one
	// for a frame after the watching goroutine replaced it.
	retired []objc.ID
}

// seekSlack is the tolerance a seek may land within, in the item's timeline.
//
// The timeline runs 1/factor times as long as the asset's, so the same slack on
// the viewer's clock is factor times it in the item's: without this, a squeezed
// timeline would allow three times as much at 3x, and the picture would jump by
// more than a second on every 拖动.
func seekSlack(factor float64) time.Duration {
	if factor <= 0 {
		factor = 1
	}
	return time.Duration(float64(seekTolerance) * factor)
}

// maxRetimeTries is how often a squeezed timeline is retried before the speed
// is given up (25 ms a tick, so about a second).
const maxRetimeTries = 40

// seekTolerance is how far from the asked-for time a seek may land, so that
// the player jumps to a key frame instead of decoding up to the exact frame.
const seekTolerance = 500 * time.Millisecond

// maxFullRate is the fastest the player shows every picture at: above it,
// AVFoundation plays the sound and keeps the clock, but hands the video
// output no picture at all (measured on Intel macOS: 65 frames in 1.2 s at
// 2.0, none at 2.5, none at 3.0, at any pitch algorithm).
const maxFullRate = 2.0

// timelineFor is how much the timeline is squeezed for a speed, and the
// rate the player then runs at: above maxFullRate the squeeze alone makes
// the speed, so that every picture is decoded.
func timelineFor(speed float64) (factor, rate float64) {
	if speed > maxFullRate {
		return 1 / speed, 1
	}
	return 1, speed
}

func newEngine(s source) (media.Engine, error) {
	if err := loadAVFoundation(); err != nil {
		return nil, err
	}
	defer pooled()()
	e := &avEngine{quit: make(chan struct{}), done: make(chan struct{}), rate: 1, factor: 1}
	var asset objc.ID
	if s.data != nil {
		if err := loadDelegate(); err != nil {
			return nil, err
		}
		e.loader = lastLoader.Add(1)
		loaders.Store(e.loader, s.data)
		url := send(class("NSURL"), "URLWithString:", nsString("mygo-video://data/"+itoa(e.loader)+".mp4"))
		asset = send(send(class("AVURLAsset"), "alloc"), "initWithURL:options:", url, objc.ID(0))
		e.delegate = send(send(objc.ID(loaderClass), "alloc"), "init")
		send(send(asset, "resourceLoader"), "setDelegate:queue:", e.delegate, loaderQueue)
	} else {
		url := send(class("NSURL"), "URLWithString:", nsString(s.url))
		if url == 0 {
			return nil, errors.New("video: not a URL: " + s.url)
		}
		asset = send(send(class("AVURLAsset"), "alloc"), "initWithURL:options:", url, objc.ID(0))
	}
	e.item = send(send(class("AVPlayerItem"), "alloc"), "initWithAsset:", asset)
	e.asset = asset
	e.output = newOutput()
	send(e.item, "addOutput:", e.output)
	e.player = send(send(class("AVPlayer"), "alloc"), "initWithPlayerItem:", e.item)
	// The player pauses at the end, rather than stand at the end playing,
	// for the goroutine to see; it loops by a seek.
	send(e.player, "setActionAtItemEnd:", avActionAtEndNone)
	msgSetBool(e.player, sel("setPreventsDisplaySleepDuringVideoPlayback:"), true)
	go e.watch()
	return e, nil
}

// newOutput is a video output handing NV12 pixel buffers the renderer wraps
// as textures, without a copy through memory.
func newOutput() objc.ID {
	attrs := send(class("NSDictionary"), "dictionaryWithObjects:forKeys:count:",
		unsafe.Pointer(&[2]objc.ID{send(class("NSNumber"), "numberWithUnsignedInt:", uint32(pixelFormat420v)), send(class("NSNumber"), "numberWithBool:", true)}),
		unsafe.Pointer(&[2]uintptr{cvPixelFormatKey, cvMetalCompatibilityKey}), uint(2))
	return send(send(class("AVPlayerItemVideoOutput"), "alloc"), "initWithPixelBufferAttributes:", attrs)
}

func itoa(n uint64) string {
	var b [20]byte
	i := len(b)
	for {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
		if n == 0 {
			return string(b[i:])
		}
	}
}

// watch follows how loading and playing go, as the player reports it,
// and wakes the window when that changes: a loop seeks back at the end.
// It wakes the window for the first picture too, which only Frame takes,
// on the main thread, as a frame of the window builds: the state stays
// Loading until then, and nothing else may build one (#1830).
func (e *avEngine) watch() {
	defer close(e.done)
	runtime.LockOSThread() // for the autorelease pools
	defer runtime.UnlockOSThread()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	// When the window was last woken for a first picture, which it wakes
	// for again while none was taken: a frame woken may build before the
	// output has it at the frame's time, or while none of the window shows.
	var wokeFirst time.Time
	for {
		select {
		case <-e.quit:
			return
		case <-tick.C:
		}
		pool := poolPush()
		e.mu.Lock()
		before, beforeErr := e.state, e.err
		e.poll()
		changed := e.state != before || e.err != beforeErr
		loading := e.state == media.Loading
		wake := e.wake
		e.mu.Unlock()
		ready := msgInt(e.item, sel("status")) == avStatusReadyToPlay
		if ready && e.interval.Load() == 0 {
			// Out of mu: State, on the main thread, never waits for it.
			e.interval.Store(int64(e.frameInterval()))
		}
		if ready && loading && time.Since(wokeFirst) >= 250*time.Millisecond {
			// The output has the first picture once it has one for now,
			// which the frame woken, due a little later, takes.
			at := msgItemTimeForHostTime(e.output, sel("itemTimeForHostTime:"), caCurrentMediaTime())
			if msgHasNewBuffer(e.output, sel("hasNewPixelBufferForItemTime:"), at) {
				wokeFirst, changed = time.Now(), true
			}
		}
		poolPop(pool)
		if changed && wake != nil {
			wake()
		}
	}
}

// poll reads the item's state, under mu.
func (e *avEngine) poll() {
	if e.closed {
		return
	}
	if e.pending != 0 {
		if e.retime(e.pending) {
			e.pending, e.tries = 0, 0
		} else if e.tries++; e.tries > maxRetimeTries {
			// The duration never came: give the speed up rather than rebuilding
			// an item every tick (the picture stalled, the window kept loading).
			e.pending, e.tries = 0, 0
			e.noRetime = true
		}
		return // the item changed: read it on the next tick
	}
	if e.rebase.Load() != 0 && e.realPosition() > 0 {
		e.rebase.Store(0) // the new item seeks where playback was
	}
	switch msgInt(e.item, sel("status")) {
	case avStatusFailed:
		e.state = media.Failed
		msg := "AVFoundation could not play the video"
		if err := send(e.item, "error"); err != 0 {
			msg = goString(send(err, "localizedDescription"))
		}
		e.err = errors.New("video: " + msg)
		return
	case avStatusReadyToPlay:
	default:
		return // loading
	}
	if e.state == media.Loading && e.buf == 0 {
		// Ready once its first picture shows (Frame).
		return
	}
	cur, _ := msgCMTime(e.item, sel("currentTime")).duration()
	dur, known := msgCMTime(e.item, sel("duration")).duration()
	if known && cur >= dur-time.Millisecond && e.playing {
		if e.loop {
			e.seekExact(0)
			msgSetFloat32(e.player, sel("setRate:"), e.rate)
			return
		}
		if e.state != media.Ended {
			e.state = media.Ended
			send(e.player, "pause")
		}
		return
	}
	if e.state == media.Ended {
		return
	}
	if e.playing && msgInt(e.player, sel("timeControlStatus")) == avWaitingToPlay {
		e.state = media.Buffering
	} else {
		e.state = media.Ready
	}
}

func (e *avEngine) SetWake(wake func()) {
	e.mu.Lock()
	e.wake = wake
	e.mu.Unlock()
}

func (e *avEngine) Frame(t time.Time) (media.Picture, bool, bool) {
	defer pooled()()
	// The item and its output are taken together: a retime replaces both on
	// the watching goroutine, and the old output stays alive (see retired).
	e.mu.Lock()
	item, output := e.item, e.output
	e.mu.Unlock()
	// The picture due as the frame shows, and never one from before now:
	// a Tester's clock stands still, and the player's runs on.
	host := caCurrentMediaTime() + max(time.Until(t), 0).Seconds()
	at := msgItemTimeForHostTime(output, sel("itemTimeForHostTime:"), host)
	changed := false
	if e.buf == 0 && msgInt(item, sel("status")) != avStatusReadyToPlay {
		// The output may have a picture before the item knows its
		// duration: ready is both.
		return media.Picture{}, false, false
	}
	if msgHasNewBuffer(output, sel("hasNewPixelBufferForItemTime:"), at) {
		if buf := msgCopyBuffer(output, sel("copyPixelBufferForItemTime:itemTimeForDisplay:"), at, 0); buf != 0 {
			// What drew the last one holds it still (the renderer's
			// textures): the surface no longer shows it.
			if e.buf != 0 {
				cvBufferRelease(e.buf)
			}
			e.buf, changed = buf, true
			e.w, e.h = int(cvPixelBufferGetWidth(buf)), int(cvPixelBufferGetHeight(buf))
			m := matrixOf(buf)
			if cvPixelBufferGetPixelFormat(buf) == pixelFormat420f {
				m += scene.FullRange
			}
			e.surface = &scene.Surface{Format: scene.SurfaceNV12, Matrix: m, Native: buf}
			e.mu.Lock()
			if e.state == media.Loading {
				e.state = media.Ready
			}
			e.mu.Unlock()
		}
	}
	if e.buf == 0 {
		return media.Picture{}, false, false
	}
	s, w, h := e.surface, e.w, e.h
	return media.Picture{Surface: s, W: w, H: h, RGBA: func(dst []byte) []byte { return pixelBufferRGBA(s, dst, w, h) }}, changed, true
}

// matrixOf is the YUV matrix a pixel buffer's attachment says, or for
// none that of its size.
func matrixOf(buf uintptr) scene.YUVMatrix {
	if m := cvBufferGetAttachment(buf, cvMatrixKey, 0); m != 0 {
		switch {
		case cfEqual(m, cvMatrix709):
			return scene.BT709
		case cfEqual(m, cvMatrix601):
			return scene.BT601
		case cfEqual(m, cvMatrix2020):
			return scene.BT2020
		}
	}
	if cvPixelBufferGetHeight(buf) >= 720 {
		return scene.BT709
	}
	return scene.BT601
}

// pixelBufferRGBA converts the frame of a surface's pixel buffer, for
// the CPU renderer, reading its planes in memory.
func pixelBufferRGBA(s *scene.Surface, dst []byte, w, h int) []byte {
	buf := s.Native
	if buf == 0 || cvPixelBufferLock(buf, 1) != 0 { // kCVPixelBufferLock_ReadOnly
		return nil
	}
	defer cvPixelBufferUnlock(buf, 1)
	rows := [2]int{h, (h + 1) / 2}
	var planes scene.Surface
	planes.Format, planes.Matrix = s.Format, s.Matrix
	for p := range 2 {
		base, stride := cvPixelBufferPlaneBase(buf, uint(p)), int(cvPixelBufferPlaneStride(buf, uint(p)))
		if base == nil {
			return nil
		}
		planes.Planes[p] = scene.Plane{Pix: unsafe.Slice((*byte)(base), stride*rows[p]), Stride: stride}
	}
	return planes.RGBA(dst, w, h)
}

// frameInterval is half the time a picture shows at the video's
// nominal rate, or 1/60 s for none. It reads the asset's tracks, which
// for an AVURLAsset loads them, over the network for a URL, until the
// item is ready to play: only the watching goroutine calls it, once the
// item is.
func (e *avEngine) frameInterval() time.Duration {
	tracks := send(send(e.item, "asset"), "tracksWithMediaType:", avMediaTypeVideo)
	if tracks != 0 && msgInt(tracks, sel("count")) > 0 {
		if fps := msgFloat32(send(tracks, "firstObject"), sel("nominalFrameRate")); fps > 1 {
			// Asked for twice as often, so that a frame shows at the
			// refresh after it came due, not up to one later.
			return time.Duration(float64(time.Second) / float64(fps) / 2)
		}
	}
	return time.Second / 60
}

// Interval is 1/60 s until the watching goroutine read the video's rate,
// never touching the asset's tracks on the main thread (#1743).
func (e *avEngine) Interval() time.Duration {
	if d := e.interval.Load(); d > 0 {
		return time.Duration(d)
	}
	return time.Second / 60
}

func (e *avEngine) State() (media.State, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state, e.err
}

func (e *avEngine) Size() (int, int) { return e.w, e.h }

func (e *avEngine) Duration() time.Duration {
	d, _ := msgCMTime(e.item, sel("duration")).duration()
	return time.Duration(float64(d) / e.factor)
}

func (e *avEngine) Position() time.Duration {
	if v := e.rebase.Load(); v != 0 {
		return time.Duration(v)
	}
	return e.realPosition()
}

// realPosition is where playback is in the asset's timeline, whatever the
// item's timeline is squeezed to.
func (e *avEngine) realPosition() time.Duration {
	d, _ := msgCMTime(e.item, sel("currentTime")).duration()
	return time.Duration(float64(d) / e.factor)
}

// setItem makes an item of the asset's timeline squeezed by factor, or the
// asset's own item for 1. The sound keeps its pitch through the squeeze by
// the spectral algorithm, which covers the whole range of edit rates.
func (e *avEngine) setItem(factor float64) objc.ID {
	if factor == 1 {
		return send(send(class("AVPlayerItem"), "alloc"), "initWithAsset:", e.asset)
	}
	// A factor whose asset has no duration yet waits for poll, which runs
	// again every 25 ms: reading the asset's own duration here would load it
	// on the thread that called SetRate.
	d, ok := msgCMTime(e.item, sel("duration")).duration()
	if !ok || d <= 0 {
		return 0
	}
	comp := send(class("AVMutableComposition"), "composition")
	span := wholeOf(d)
	for _, media := range []uintptr{avMediaTypeVideo, avMediaTypeAudio} {
		tracks := send(e.asset, "tracksWithMediaType:", media)
		if tracks == 0 || msgInt(tracks, sel("count")) == 0 {
			continue
		}
		track := send(comp, "addMutableTrackWithMediaType:preferredTrackID:", media, int32(0))
		if track == 0 {
			continue
		}
		msgInsertTimeRange(track, sel("insertTimeRange:ofTrack:atTime:error:"), span, send(tracks, "firstObject"), span.Start, nil)
	}
	msgScaleTimeRange(comp, sel("scaleTimeRange:toDuration:"), span, cmTimeOf(time.Duration(float64(d)*factor)))
	item := send(send(class("AVPlayerItem"), "alloc"), "initWithAsset:", comp)
	send(item, "setAudioTimePitchAlgorithm:", avPitchSpectral)
	return item
}

// retime switches the item to a timeline running 1/factor times as long,
// keeping the position and the playing state: a squeezed timeline is how
// the player shows every picture above maxFullRate, where it hands the
// video output none. It runs under mu.
func (e *avEngine) retime(factor float64) bool {
	item := e.setItem(factor)
	if item == 0 {
		return false
	}
	pos := e.realPosition()
	send(e.item, "removeOutput:", e.output)
	e.retired = append(e.retired, e.output)
	e.output = newOutput()
	send(item, "addOutput:", e.output)
	send(e.player, "replaceCurrentItemWithPlayerItem:", item)
	send(e.item, "release")
	e.item, e.factor = item, factor
	if pos > 0 {
		e.rebase.Store(int64(pos))
		// A tolerant seek: this runs right after a new item was made, and an
		// exact seek on a long video decodes from the key frame - seconds of
		// work while the window shows the old picture.
		e.seek(pos)
	}
	if e.playing {
		msgSetFloat32(e.player, sel("setRate:"), e.rate)
	}
	return true
}

func (e *avEngine) Play() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.playing = true
	if e.state == media.Ended {
		e.state = media.Ready
		e.seekExact(0)
	}
	msgSetFloat32(e.player, sel("setRate:"), e.rate)
}

func (e *avEngine) Pause() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.playing = false
	send(e.player, "pause")
}

func (e *avEngine) Seek(d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.state == media.Ended {
		e.state = media.Ready
	}
	e.seek(d)
}

// seek seeks to d, in the asset's timeline, under mu, allowing the player to
// land within seekTolerance of it.
//
// The tolerance is what a viewer asks for from a progress bar: an exact seek
// decodes from the key frame before the target, which on a long video takes
// tens of milliseconds and stalls the thread that asked (dragging seeks on
// every move). Half a second is under what a viewer notices.
func (e *avEngine) seek(d time.Duration) {
	tolerance := cmTimeOf(seekSlack(e.factor))
	msgSeek(e.player, sel("seekToTime:toleranceBefore:toleranceAfter:"), cmTimeOf(time.Duration(float64(d)*e.factor)), tolerance, tolerance)
}

// seekExact seeks to d exactly, under mu, for the seeks the engine makes on
// its own: where playback continues after a retime or a loop must not drift
// by the tolerance the progress bar allows.
func (e *avEngine) seekExact(d time.Duration) {
	zero := cmTime{Flags: cmTimeValid, Timescale: 1}
	msgSeek(e.player, sel("seekToTime:toleranceBefore:toleranceAfter:"), cmTimeOf(time.Duration(float64(d)*e.factor)), zero, zero)
}

func (e *avEngine) SetLoop(loop bool) {
	e.mu.Lock()
	e.loop = loop
	e.mu.Unlock()
}

func (e *avEngine) SetRate(r float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	factor, rate := timelineFor(r)
	e.rate = float32(rate)
	if factor == e.factor {
		e.pending = 0
		if e.playing && e.state != media.Ended {
			msgSetFloat32(e.player, sel("setRate:"), e.rate)
		}
		return
	}
	// The timeline has to change, which builds a composition and seeks: too
	// much for the thread that called this, which is the one drawing the
	// window. Pause now and let the watching goroutine do it (see poll).
	if e.noRetime {
		// The timeline cannot be squeezed for this asset: play at the fastest
		// full-frame rate instead of freezing.
		e.rate = float32(maxFullRate)
		if e.playing && e.state != media.Ended {
			msgSetFloat32(e.player, sel("setRate:"), e.rate)
		}
		return
	}
	e.pending, e.tries = factor, 0
	send(e.player, "pause")
}

func (e *avEngine) SetVolume(v float64) { msgSetFloat32(e.player, sel("setVolume:"), float32(v)) }

func (e *avEngine) SetMuted(m bool) { msgSetBool(e.player, sel("setMuted:"), m) }

// Close stops the player and frees it. The pixel buffer showing goes
// once nothing shows its surface any more.
func (e *avEngine) Close() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	e.wake = nil
	e.mu.Unlock()
	close(e.quit)
	<-e.done
	pop := pooled()
	send(e.player, "pause")
	send(e.player, "replaceCurrentItemWithPlayerItem:", objc.ID(0))
	send(e.item, "removeOutput:", e.output)
	send(e.output, "release")
	send(e.item, "release")
	send(e.player, "release")
	send(e.asset, "release")
	for _, out := range e.retired {
		send(out, "release")
	}
	e.retired = nil
	if e.delegate != 0 {
		send(e.delegate, "release")
		loaders.Delete(e.loader)
	}
	pop()
	if e.buf != 0 {
		buf := e.buf
		e.buf = 0
		runtime.AddCleanup(e.surface, func(b uintptr) { cvBufferRelease(b) }, buf)
	}
	return nil
}

// loadDelegate registers the class of the delegates that answer an
// AVAssetResourceLoader from bytes in memory, on a queue of their own.
func loadDelegate() error {
	if loaderClass != 0 {
		return nil
	}
	// The protocol is in the runtime only if something compiled against
	// it loaded: a nil one in the class's list would crash the runtime
	// when AVFoundation asks the class for a method's signature.
	var protocols []*objc.Protocol
	if p := objc.GetProtocol("AVAssetResourceLoaderDelegate"); p != nil {
		protocols = append(protocols, p)
	}
	var err error
	loaderClass, err = objc.RegisterClass("MyGoVideoDataLoader", objc.GetClass("NSObject"),
		protocols, nil,
		[]objc.MethodDef{{
			Cmd: sel("resourceLoader:shouldWaitForLoadingOfRequestedResource:"),
			Fn:  answerLoading,
		}})
	if err != nil {
		return err
	}
	loaderQueue = dispatchQueueCreate("mygo.video.data", 0)
	return nil
}

// answerLoading answers a request for bytes of a video in memory: what
// they are, and those asked for.
func answerLoading(self objc.ID, _ objc.SEL, loader, req objc.ID) bool {
	url := goString(send(send(send(req, "request"), "URL"), "absoluteString"))
	var n uint64
	for i := len("mygo-video://data/"); i < len(url) && '0' <= url[i] && url[i] <= '9'; i++ {
		n = n*10 + uint64(url[i]-'0')
	}
	v, ok := loaders.Load(n)
	if !ok {
		return false
	}
	data := v.([]byte)
	if info := send(req, "contentInformationRequest"); info != 0 {
		send(info, "setContentType:", nsString(contentType(data)))
		send(info, "setContentLength:", int64(len(data)))
		msgSetBool(info, sel("setByteRangeAccessSupported:"), true)
	}
	if dr := send(req, "dataRequest"); dr != 0 {
		off := int64(send(dr, "currentOffset"))
		end := int64(send(dr, "requestedOffset")) + int64(send(dr, "requestedLength"))
		if msgBool(dr, sel("requestsAllDataToEndOfResource")) {
			end = int64(len(data))
		}
		off, end = min(max(off, 0), int64(len(data))), min(end, int64(len(data)))
		if end > off {
			chunk := send(class("NSData"), "dataWithBytes:length:", unsafe.Pointer(&data[off]), uint(end-off))
			send(dr, "respondWithData:", chunk)
		}
	}
	send(req, "finishLoading")
	return true
}

// contentType is the UTI of a video's bytes, as their container says.
func contentType(b []byte) string {
	if len(b) >= 12 && string(b[4:8]) == "ftyp" && string(b[8:12]) == "qt  " {
		return "com.apple.quicktime-movie"
	}
	return "public.mpeg-4"
}
