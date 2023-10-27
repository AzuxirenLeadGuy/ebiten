// Copyright 2016 Hajime Hoshi
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build android || ios

package ui

import (
	stdcontext "context"
	"fmt"
	"image"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"unicode"

	"golang.org/x/mobile/app"
	"golang.org/x/mobile/event/key"
	"golang.org/x/mobile/event/lifecycle"
	"golang.org/x/mobile/event/paint"
	"golang.org/x/mobile/event/size"
	"golang.org/x/mobile/event/touch"
	"golang.org/x/mobile/gl"

	"github.com/hajimehoshi/ebiten/v2/internal/gamepad"
	"github.com/hajimehoshi/ebiten/v2/internal/graphicscommand"
	"github.com/hajimehoshi/ebiten/v2/internal/graphicsdriver"
	"github.com/hajimehoshi/ebiten/v2/internal/hook"
	"github.com/hajimehoshi/ebiten/v2/internal/restorable"
	"github.com/hajimehoshi/ebiten/v2/internal/thread"
)

var (
	glContextCh = make(chan gl.Context, 1)

	// renderCh receives when updating starts.
	renderCh = make(chan struct{})

	// renderEndCh receives when updating finishes.
	renderEndCh = make(chan struct{})
)

func (u *UserInterface) init() error {
	u.userInterfaceImpl = userInterfaceImpl{
		foreground:            1,
		graphicsLibraryInitCh: make(chan struct{}),
		errCh:                 make(chan error),

		// Give a default outside size so that the game can start without initializing them.
		outsideWidth:  640,
		outsideHeight: 480,
	}
	return nil
}

// Update is called from mobile/ebitenmobileview.
//
// Update must be called on the rendering thread.
func (u *UserInterface) Update() error {
	select {
	case err := <-u.errCh:
		return err
	default:
	}

	if !u.IsFocused() {
		return nil
	}

	if err := gamepad.Update(); err != nil {
		return err
	}

	ctx, cancel := stdcontext.WithCancel(stdcontext.Background())
	defer cancel()

	renderCh <- struct{}{}
	go func() {
		<-renderEndCh
		cancel()
	}()

	_ = u.renderThread.Loop(ctx)
	return nil
}

type userInterfaceImpl struct {
	graphicsDriver        graphicsdriver.Graphics
	graphicsLibraryInitCh chan struct{}

	outsideWidth  float64
	outsideHeight float64

	deviceScaleFactorOnce sync.Once
	deviceScaleFactor     float64

	foreground int32
	errCh      chan error

	// Used for gomobile-build
	gbuildWidthPx   int
	gbuildHeightPx  int
	setGBuildSizeCh chan struct{}
	once            sync.Once

	context *context

	inputState InputState
	touches    []TouchForInput

	fpsMode         int32
	renderRequester RenderRequester

	renderThread *thread.OSThread

	m sync.RWMutex
}

// appMain is the main routine for gomobile-build mode.
func (u *UserInterface) appMain(a app.App) {
	var glctx gl.Context
	var sizeInited bool

	touches := map[touch.Sequence]TouchForInput{}
	keys := map[Key]struct{}{}

	for e := range a.Events() {
		var updateInput bool
		var runes []rune

		switch e := a.Filter(e).(type) {
		case lifecycle.Event:
			switch e.Crosses(lifecycle.StageVisible) {
			case lifecycle.CrossOn:
				if err := u.SetForeground(true); err != nil {
					// There are no other ways than panicking here.
					panic(err)
				}
				restorable.OnContextLost()
				glctx, _ = e.DrawContext.(gl.Context)
				// Assume that glctx is always a same instance.
				// Then, only once initializing should be enough.
				if glContextCh != nil {
					glContextCh <- glctx
					glContextCh = nil
				}
				a.Send(paint.Event{})
			case lifecycle.CrossOff:
				if err := u.SetForeground(false); err != nil {
					// There are no other ways than panicking here.
					panic(err)
				}
				glctx = nil
			}
		case size.Event:
			u.setGBuildSize(e.WidthPx, e.HeightPx)
			sizeInited = true
		case paint.Event:
			if !sizeInited {
				a.Send(paint.Event{})
				continue
			}
			if glctx == nil || e.External {
				continue
			}
			renderCh <- struct{}{}
			<-renderEndCh
			a.Publish()
			a.Send(paint.Event{})
		case touch.Event:
			if !sizeInited {
				continue
			}
			switch e.Type {
			case touch.TypeBegin, touch.TypeMove:
				s := u.DeviceScaleFactor()
				touches[e.Sequence] = TouchForInput{
					ID: TouchID(e.Sequence),
					X:  float64(e.X) / s,
					Y:  float64(e.Y) / s,
				}
			case touch.TypeEnd:
				delete(touches, e.Sequence)
			}
			updateInput = true
		case key.Event:
			k, ok := gbuildKeyToUIKey[e.Code]
			if ok {
				switch e.Direction {
				case key.DirPress, key.DirNone:
					keys[k] = struct{}{}
				case key.DirRelease:
					delete(keys, k)
				}
			}

			switch e.Direction {
			case key.DirPress, key.DirNone:
				if e.Rune != -1 && unicode.IsPrint(e.Rune) {
					runes = []rune{e.Rune}
				}
			}
			updateInput = true
		}

		if updateInput {
			var ts []TouchForInput
			for _, t := range touches {
				ts = append(ts, t)
			}
			u.updateInputStateFromOutside(keys, runes, ts)
		}
	}
}

func (u *UserInterface) SetForeground(foreground bool) error {
	var v int32
	if foreground {
		v = 1
	}
	atomic.StoreInt32(&u.foreground, v)

	if foreground {
		return hook.ResumeAudio()
	} else {
		return hook.SuspendAudio()
	}
}

func (u *UserInterface) Run(game Game, options *RunOptions) error {
	u.setGBuildSizeCh = make(chan struct{})
	go func() {
		if err := u.run(game, true, options); err != nil {
			// As mobile apps never ends, Loop can't return. Just panic here.
			panic(err)
		}
	}()
	app.Main(u.appMain)
	return nil
}

func (u *UserInterface) RunWithoutMainLoop(game Game, options *RunOptions) {
	go func() {
		if err := u.run(game, false, options); err != nil {
			u.errCh <- err
		}
	}()
}

func (u *UserInterface) run(game Game, mainloop bool, options *RunOptions) (err error) {
	// Convert the panic to a regular error so that Java/Objective-C layer can treat this easily e.g., for
	// Crashlytics. A panic is treated as SIGABRT, and there is no way to handle this on Java/Objective-C layer
	// unfortunately.
	// TODO: Panic on other goroutines cannot be handled here.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v\n%s", r, string(debug.Stack()))
		}
	}()

	var mgl gl.Context
	if mainloop {
		// When gomobile-build is used, GL functions must be called via
		// gl.Context so that they are called on the appropriate thread.
		mgl = <-glContextCh
	} else {
		u.renderThread = thread.NewOSThread()
		graphicscommand.SetRenderThread(u.renderThread)
	}

	u.setRunning(true)
	defer u.setRunning(false)

	u.context = newContext(game)

	g, lib, err := newGraphicsDriver(&graphicsDriverCreatorImpl{
		gomobileContext: mgl,
	}, options.GraphicsLibrary)
	if err != nil {
		return err
	}
	u.graphicsDriver = g
	u.setGraphicsLibrary(lib)
	close(u.graphicsLibraryInitCh)

	// If gomobile-build is used, wait for the outside size fixed.
	if u.setGBuildSizeCh != nil {
		<-u.setGBuildSizeCh
	}

	for {
		if err := u.update(); err != nil {
			return err
		}
	}
}

// outsideSize must be called on the same goroutine as update().
func (u *UserInterface) outsideSize() (float64, float64) {
	var outsideWidth, outsideHeight float64

	u.m.RLock()
	if u.gbuildWidthPx == 0 || u.gbuildHeightPx == 0 {
		outsideWidth = u.outsideWidth
		outsideHeight = u.outsideHeight
	} else {
		// gomobile build
		d := u.DeviceScaleFactor()
		outsideWidth = float64(u.gbuildWidthPx) / d
		outsideHeight = float64(u.gbuildHeightPx) / d
	}
	u.m.RUnlock()

	return outsideWidth, outsideHeight
}

func (u *UserInterface) update() error {
	<-renderCh
	defer func() {
		renderEndCh <- struct{}{}
	}()

	w, h := u.outsideSize()
	if err := u.context.updateFrame(u.graphicsDriver, w, h, u.DeviceScaleFactor(), u, nil); err != nil {
		return err
	}
	return nil
}

func (u *UserInterface) ScreenSizeInFullscreen() (int, int) {
	// TODO: This function should return gbuildWidthPx, gbuildHeightPx,
	// but these values are not initialized until the main loop starts.
	return 0, 0
}

// SetOutsideSize is called from mobile/ebitenmobileview.
//
// SetOutsideSize is concurrent safe.
func (u *UserInterface) SetOutsideSize(outsideWidth, outsideHeight float64) {
	u.m.Lock()
	if u.outsideWidth != outsideWidth || u.outsideHeight != outsideHeight {
		u.outsideWidth = outsideWidth
		u.outsideHeight = outsideHeight
	}
	u.m.Unlock()
}

func (u *UserInterface) setGBuildSize(widthPx, heightPx int) {
	u.m.Lock()
	u.gbuildWidthPx = widthPx
	u.gbuildHeightPx = heightPx
	u.m.Unlock()

	u.once.Do(func() {
		close(u.setGBuildSizeCh)
	})
}

func (u *UserInterface) CursorMode() CursorMode {
	return CursorModeHidden
}

func (u *UserInterface) SetCursorMode(mode CursorMode) {
	// Do nothing
}

func (u *UserInterface) CursorShape() CursorShape {
	return CursorShapeDefault
}

func (u *UserInterface) SetCursorShape(shape CursorShape) {
	// Do nothing
}

func (u *UserInterface) IsFullscreen() bool {
	return false
}

func (u *UserInterface) SetFullscreen(fullscreen bool) {
	// Do nothing
}

func (u *UserInterface) IsFocused() bool {
	return atomic.LoadInt32(&u.foreground) != 0
}

func (u *UserInterface) IsRunnableOnUnfocused() bool {
	return false
}

func (u *UserInterface) SetRunnableOnUnfocused(runnableOnUnfocused bool) {
	// Do nothing
}

func (u *UserInterface) FPSMode() FPSModeType {
	return FPSModeType(atomic.LoadInt32(&u.fpsMode))
}

func (u *UserInterface) SetFPSMode(mode FPSModeType) {
	atomic.StoreInt32(&u.fpsMode, int32(mode))
	u.updateExplicitRenderingModeIfNeeded(mode)
}

func (u *UserInterface) updateExplicitRenderingModeIfNeeded(fpsMode FPSModeType) {
	if u.renderRequester == nil {
		return
	}
	u.renderRequester.SetExplicitRenderingMode(fpsMode == FPSModeVsyncOffMinimum)
}

func (u *UserInterface) DeviceScaleFactor() float64 {
	// Assume that the device scale factor never changes on mobiles.
	u.deviceScaleFactorOnce.Do(func() {
		u.deviceScaleFactor = deviceScaleFactorImpl()
	})
	return u.deviceScaleFactor
}

func (u *UserInterface) readInputState(inputState *InputState) {
	u.m.Lock()
	defer u.m.Unlock()
	u.inputState.copyAndReset(inputState)
}

func (u *UserInterface) Window() Window {
	return &nullWindow{}
}

type Monitor struct{}

var theMonitor = &Monitor{}

func (m *Monitor) Bounds() image.Rectangle {
	// TODO: This should return the available viewport dimensions.
	return image.Rectangle{}
}

func (m *Monitor) Name() string {
	return ""
}

func (u *UserInterface) AppendMonitors(mons []*Monitor) []*Monitor {
	return append(mons, theMonitor)
}

func (u *UserInterface) Monitor() *Monitor {
	return theMonitor
}

func (u *UserInterface) UpdateInput(keys map[Key]struct{}, runes []rune, touches []TouchForInput) {
	u.updateInputStateFromOutside(keys, runes, touches)
	if FPSModeType(atomic.LoadInt32(&u.fpsMode)) == FPSModeVsyncOffMinimum {
		u.renderRequester.RequestRenderIfNeeded()
	}
}

type RenderRequester interface {
	SetExplicitRenderingMode(explicitRendering bool)
	RequestRenderIfNeeded()
}

func (u *UserInterface) SetRenderRequester(renderRequester RenderRequester) {
	u.renderRequester = renderRequester
	u.updateExplicitRenderingModeIfNeeded(FPSModeType(atomic.LoadInt32(&u.fpsMode)))
}

func (u *UserInterface) ScheduleFrame() {
	if u.renderRequester != nil && FPSModeType(atomic.LoadInt32(&u.fpsMode)) == FPSModeVsyncOffMinimum {
		u.renderRequester.RequestRenderIfNeeded()
	}
}

func (u *UserInterface) beginFrame() {
}

func (u *UserInterface) endFrame() {
}

func (u *UserInterface) updateIconIfNeeded() error {
	return nil
}

func IsScreenTransparentAvailable() bool {
	return false
}
