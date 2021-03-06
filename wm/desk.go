// +build linux

package wm // import "fyne.io/fynedesk/wm"

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"time"

	"github.com/BurntSushi/xgb/xproto"
	"github.com/BurntSushi/xgbutil"
	"github.com/BurntSushi/xgbutil/ewmh"
	"github.com/BurntSushi/xgbutil/icccm"
	"github.com/BurntSushi/xgbutil/xevent"
	"github.com/BurntSushi/xgbutil/xprop"

	"fyne.io/fyne"

	"fyne.io/fynedesk"
	"fyne.io/fynedesk/internal/notify"
	"fyne.io/fynedesk/internal/ui"
)

type x11WM struct {
	stack
	x                       *xgbutil.XUtil
	root                    fyne.Window
	rootID                  xproto.Window
	loaded                  bool
	moveResizing            bool
	moveResizingStartX      int16
	moveResizingStartY      int16
	moveResizingLastX       int16
	moveResizingLastY       int16
	moveResizingStartWidth  uint16
	moveResizingStartHeight uint16
	moveResizingType        moveResizeType
	altTabList              []fynedesk.Window
	altTabIndex             int

	allowedActions []string
	supportedHints []string
	transientMap   map[xproto.Window][]xproto.Window
}

type moveResizeType uint32

const (
	moveResizeTopLeft      moveResizeType = 0
	moveResizeTop          moveResizeType = 1
	moveResizeTopRight     moveResizeType = 2
	moveResizeRight        moveResizeType = 3
	moveResizeBottomRight  moveResizeType = 4
	moveResizeBottom       moveResizeType = 5
	moveResizeBottomLeft   moveResizeType = 6
	moveResizeLeft         moveResizeType = 7
	moveResizeMove         moveResizeType = 8
	moveResizeKeyboard     moveResizeType = 9
	moveResizeMoveKeyboard moveResizeType = 10
	moveResizeCancel       moveResizeType = 11

	keyCodeTab   = 23
	keyCodeAlt   = 64
	keyCodeSpace = 65
)

func (x *x11WM) Close() {
	log.Println("Disconnecting from X server")

	for _, child := range x.clients {
		child.(*client).frame.unFrame()
	}

	x.x.Conn().Close()
}

func (x *x11WM) AddStackListener(l fynedesk.StackListener) {
	x.stack.listeners = append(x.stack.listeners, l)
}

func (x *x11WM) SetRoot(win fyne.Window) {
	x.root = win
}

func (x *x11WM) Blank() {
	go func() {
		time.Sleep(time.Second / 3)
		exec.Command("xset", "-display", os.Getenv("DISPLAY"), "dpms", "force", "off").Start()
	}()
}

// NewX11WindowManager sets up a new X11 Window Manager to control a desktop in X11.
func NewX11WindowManager(a fyne.App) (fynedesk.WindowManager, error) {
	conn, err := xgbutil.NewConn()
	if err != nil {
		fyne.LogError("Failed to connect to the XServer", err)
		return nil, err
	}

	mgr := &x11WM{x: conn}
	root := conn.RootWin()
	mgr.takeSelectionOwnership()
	mgr.transientMap = make(map[xproto.Window][]xproto.Window)

	eventMask := xproto.EventMaskPropertyChange |
		xproto.EventMaskFocusChange |
		xproto.EventMaskButtonPress |
		xproto.EventMaskButtonRelease |
		xproto.EventMaskKeyPress |
		xproto.EventMaskStructureNotify |
		xproto.EventMaskSubstructureRedirect
	if err := xproto.ChangeWindowAttributesChecked(conn.Conn(), root, xproto.CwEventMask,
		[]uint32{uint32(eventMask)}).Check(); err != nil {
		conn.Conn().Close()

		return nil, errors.New("window manager detected, running embedded")
	}

	mgr.allowedActions = []string{
		"_NET_WM_ACTION_MOVE",
		"_NET_WM_ACTION_RESIZE",
		"_NET_WM_ACTION_MINIMIZE",
		"_NET_WM_ACTION_MAXIMIZE_HORZ",
		"_NET_WM_ACTION_MAXIMIZE_VERT",
		"_NET_WM_ACTION_CLOSE",
		"_NET_WM_ACTION_FULLSCREEN",
	}

	mgr.supportedHints = append(mgr.supportedHints, mgr.allowedActions...)
	mgr.supportedHints = append(mgr.supportedHints, "_NET_ACTIVE_WINDOW",
		"_NET_CLIENT_LIST",
		"_NET_CLIENT_LIST_STACKING",
		"_NET_CURRENT_DESKTOP",
		"_NET_DESKTOP_GEOMETRY",
		"_NET_DESKTOP_VIEWPORT",
		"_NET_FRAME_EXTENTS",
		"_NET_MOVERESIZE_WINDOW",
		"_NET_NUMBER_OF_DESKTOPS",
		"_NET_WM_FULLSCREEN_MONITORS",
		"_NET_WM_MOVERESIZE",
		"_NET_WM_NAME",
		"_NET_WM_STATE",
		"_NET_WM_STATE_FULLSCREEN",
		"_NET_WM_STATE_HIDDEN",
		"_NET_WM_STATE_MAXIMIZED_HORZ",
		"_NET_WM_STATE_MAXIMIZED_VERT",
		"_NET_WM_STATE_SKIP_PAGER",
		"_NET_WM_STATE_SKIP_TASKBAR",
		"_NET_WORKAREA",
		"_NET_SUPPORTED")

	ewmh.SupportedSet(mgr.x, mgr.supportedHints)
	ewmh.SupportingWmCheckSet(mgr.x, mgr.x.RootWin(), mgr.x.Dummy())
	ewmh.SupportingWmCheckSet(mgr.x, mgr.x.Dummy(), mgr.x.Dummy())
	ewmh.WmNameSet(mgr.x, mgr.x.Dummy(), "Fyne Desktop")
	ewmh.DesktopViewportSet(mgr.x, []ewmh.DesktopViewport{{X: 0, Y: 0}}) // Will always be 0, 0 until virtual desktops are supported
	ewmh.NumberOfDesktopsSet(mgr.x, 1)                                   // Will always be 1 until virtual desktops are supported
	ewmh.CurrentDesktopSet(mgr.x, 0)                                     // Will always be 0 until virtual desktops are supported

	loadCursors(conn)
	go mgr.runLoop()

	listener := make(chan fyne.Settings)
	a.Settings().AddChangeListener(listener)
	go func() {
		for {
			<-listener
			for _, c := range mgr.clients {
				c.(*client).frame.applyTheme(true)
			}
		}
	}()

	return mgr, nil
}

func (x *x11WM) runLoop() {
	conn := x.x.Conn()

	for {
		ev, err := conn.WaitForEvent()
		if err != nil {
			fyne.LogError("X11 Error:", err)
			continue
		}
		if ev == nil { // disconnected if both are nil
			break
		}
		switch ev := ev.(type) {
		case xproto.MapRequestEvent:
			x.showWindow(ev.Window, ev.Parent)
		case xproto.UnmapNotifyEvent:
			x.hideWindow(ev.Window)
		case xproto.VisibilityNotifyEvent:
			x.handleVisibilityChange(ev)
		case xproto.ConfigureRequestEvent:
			x.configureWindow(ev.Window, ev)
		case xproto.ConfigureNotifyEvent:
			if ev.Window != x.x.RootWin() {
				break
			}
			xproto.ConfigureWindowChecked(x.x.Conn(), x.rootID, xproto.ConfigWindowX|xproto.ConfigWindowY|
				xproto.ConfigWindowWidth|xproto.ConfigWindowHeight,
				[]uint32{uint32(ev.X), uint32(ev.Y), uint32(ev.Width), uint32(ev.Height)}).Check()
		case xproto.CreateNotifyEvent:
			err := xproto.ChangeWindowAttributesChecked(x.x.Conn(), ev.Window, xproto.CwCursor,
				[]uint32{uint32(defaultCursor)}).Check()
			if err != nil {
				fyne.LogError("Set Cursor Error", err)
			}
		case xproto.DestroyNotifyEvent:
			x.destroyWindow(ev.Window)
		case xproto.FocusInEvent:
			x.handleFocus(ev.Event)
		case xproto.FocusOutEvent:
			x.handleFocus(ev.Event)
		case xproto.PropertyNotifyEvent:
			x.handlePropertyChange(ev)
		case xproto.ClientMessageEvent:
			x.handleClientMessage(ev)
		case xproto.ExposeEvent:
			border := x.clientForWin(ev.Window)
			if border != nil && border.(*client).frame != nil {
				border.(*client).frame.applyTheme(false)
			}
		case xproto.ButtonPressEvent:
			for _, c := range x.clients {
				if c.(*client).id == ev.Event {
					c.(*client).frame.press(ev.RootX, ev.RootY)
				}
			}
			xevent.ReplayPointer(x.x)
		case xproto.ButtonReleaseEvent:
			for _, c := range x.clients {
				if c.(*client).id == ev.Event {
					if !x.moveResizing {
						c.(*client).frame.release(ev.RootX, ev.RootY)
					}
					x.moveResizeEnd(c.(*client))
				}
			}
		case xproto.MotionNotifyEvent:
			for _, c := range x.clients {
				if c.(*client).id == ev.Event {
					if x.moveResizing {
						x.moveResize(ev.RootX, ev.RootY, c.(*client))
						break
					}
					if ev.State&xproto.ButtonMask1 != 0 {
						c.(*client).frame.drag(ev.RootX, ev.RootY)
					} else {
						c.(*client).frame.motion(ev.RootX, ev.RootY)
					}
					break
				}
			}
		case xproto.EnterNotifyEvent:
			err := xproto.ChangeWindowAttributesChecked(x.x.Conn(), ev.Event, xproto.CwCursor,
				[]uint32{uint32(defaultCursor)}).Check()
			if err != nil {
				fyne.LogError("Set Cursor Error", err)
			}
			if mouseNotify, ok := fynedesk.Instance().(notify.MouseNotify); ok {
				mouseNotify.MouseOutNotify()
			}
		case xproto.LeaveNotifyEvent:
			if mouseNotify, ok := fynedesk.Instance().(notify.MouseNotify); ok {
				mouseNotify.MouseInNotify(fyne.NewPos(int(float32(ev.RootX)/x.root.Canvas().Scale()),
					int(float32(ev.RootY)/x.root.Canvas().Scale())))
			}
		case xproto.KeyPressEvent:
			if ev.Detail == keyCodeSpace {
				go ui.ShowAppLauncher()
				break
			} else if ev.Detail != keyCodeTab {
				break
			}
			if x.altTabList == nil {
				x.altTabList = []fynedesk.Window{}
				for _, win := range x.Windows() {
					if win.Iconic() {
						continue
					}
					x.altTabList = append(x.altTabList, win)
				}
				x.altTabIndex = 0

				xproto.GrabKeyboard(x.x.Conn(), true, x.rootID, xproto.TimeCurrentTime, xproto.GrabModeAsync, xproto.GrabModeAsync)
			}

			winCount := len(x.altTabList)
			if winCount <= 1 {
				break
			}
			if ev.State&xproto.ModMaskShift != 0 {
				x.altTabIndex--
				if x.altTabIndex < 0 {
					x.altTabIndex = winCount - 1
				}
			} else {
				x.altTabIndex++
				if x.altTabIndex == winCount {
					x.altTabIndex = 0
				}
			}

			x.RaiseToTop(x.altTabList[x.altTabIndex])
			windowClientListStackingUpdate(x)
		case xproto.KeyReleaseEvent:
			if ev.Detail == keyCodeAlt {
				x.altTabList = nil
				xproto.UngrabKeyboard(x.x.Conn(), xproto.TimeCurrentTime)
			}
		}
	}

	fyne.LogError("X11 connection terminated!", nil)
}

func (x *x11WM) configureWindow(win xproto.Window, ev xproto.ConfigureRequestEvent) {
	c := x.clientForWin(win)
	width := ev.Width
	height := ev.Height

	if c != nil {
		f := c.(*client).frame
		if f != nil && c.(*client).win == win { // ignore requests from our frame as we must have caused it
			f.minWidth, f.minHeight = windowSizeMin(x.x, win)
			if c.Decorated() {
				err := xproto.ConfigureWindowChecked(x.x.Conn(), win, xproto.ConfigWindowX|xproto.ConfigWindowY|
					xproto.ConfigWindowWidth|xproto.ConfigWindowHeight,
					[]uint32{uint32(f.borderWidth()), uint32(f.titleHeight()),
						uint32(width), uint32(height)}).Check()

				if err != nil {
					fyne.LogError("Configure Frame Error", err)
				}
			} else {
				if ev.X == 0 && ev.Y == 0 {
					ev.X = f.x
					ev.Y = f.y
				}
				c.(*client).setWindowGeometry(ev.X, ev.Y, ev.Width, ev.Height)
			}
		}
		return
	}

	name := windowName(x.x, win)
	isRoot := x.root != nil && name == x.root.Title()
	if isRoot {
		width = x.x.Screen().WidthInPixels
		height = x.x.Screen().HeightInPixels

		ewmh.DesktopGeometrySet(x.x, &ewmh.DesktopGeometry{Width: int(width), Height: int(height)})    // The array will grow when virtual desktops are supported
		ewmh.WorkareaSet(x.x, []ewmh.Workarea{{X: 0, Y: 0, Width: uint(width), Height: uint(height)}}) // The array will grow when virtual desktops are supported
	}
	err := xproto.ConfigureWindowChecked(x.x.Conn(), win, xproto.ConfigWindowX|xproto.ConfigWindowY|
		xproto.ConfigWindowWidth|xproto.ConfigWindowHeight,
		[]uint32{uint32(ev.X), uint32(ev.Y), uint32(width), uint32(height)}).Check()
	if err != nil {
		fyne.LogError("Configure Window Error", err)
	}

	if isRoot {
		if x.loaded {
			return
		}
		x.rootID = win
		x.loaded = true
	}
}

func (x *x11WM) handleFocus(win xproto.Window) {
	c := x.clientForWin(win)
	if c == nil {
		return
	}
	c.(*client).frame.applyTheme(true)
}

func (x *x11WM) handlePropertyChange(ev xproto.PropertyNotifyEvent) {
	c := x.clientForWin(ev.Window)
	if c == nil {
		return
	}
	propAtom, err := xprop.AtomName(x.x, ev.Atom)
	if err != nil {
		fyne.LogError("Error getting event", err)
		return
	}
	switch propAtom {
	case "_NET_WM_NAME":
		c.(*client).updateTitle()
	case "WM_NAME":
		c.(*client).updateTitle()
	case "WM_NORMAL_HINTS":
		// Force a reconfigure to make sure the client is constrained to the new size hints
		x, y, width, height := c.(*client).getWindowGeometry()
		c.(*client).setWindowGeometry(x, y, width, height)
	case "_MOTIF_WM_HINTS":
		c.(*client).setupBorder()
	}
}

func (x *x11WM) moveResizeEnd(c *client) {
	x.moveResizing = false
	xproto.UngrabPointer(x.x.Conn(), xproto.TimeCurrentTime)
	xproto.UngrabKeyboard(x.x.Conn(), xproto.TimeCurrentTime)

	// ensure menus etc update
	c.frame.notifyInnerGeometry()
}

func (x *x11WM) moveResize(moveX, moveY int16, c *client) {
	xcoord, ycoord, width, height := c.getWindowGeometry()
	w := int16(width)
	h := int16(height)
	deltaW := moveX - x.moveResizingLastX
	deltaH := moveY - x.moveResizingLastY
	deltaX := moveX - x.moveResizingStartX
	deltaY := moveY - x.moveResizingStartY

	switch x.moveResizingType {
	case moveResizeTopLeft:
		//Move both X,Y coords and resize both W,H
		xcoord += deltaW
		ycoord += deltaH

		w = int16(x.moveResizingStartWidth) - deltaX
		h = int16(x.moveResizingStartHeight) - deltaY
	case moveResizeTop:
		//Move Y coord and resize H
		ycoord += deltaH
		h = int16(x.moveResizingStartHeight) - deltaY
	case moveResizeTopRight:
		//Move Y coord and resize both W,H
		ycoord += deltaH
		w = int16(x.moveResizingStartWidth) + deltaX
		h = int16(x.moveResizingStartHeight) - deltaY
	case moveResizeRight:
		//Keep X coord and resize W
		w = int16(x.moveResizingStartWidth) + deltaX
	case moveResizeBottomRight, moveResizeKeyboard:
		//Keep both X,Y coords and resize both W,H
		w = int16(x.moveResizingStartWidth) + deltaX
		h = int16(x.moveResizingStartHeight) + deltaY
	case moveResizeBottom:
		//Keep Y coord and resize H
		h = int16(x.moveResizingStartHeight) + deltaY
	case moveResizeBottomLeft:
		//Move X coord and resize both W,H
		xcoord += deltaW
		w = int16(x.moveResizingStartWidth) - deltaX
		h = int16(x.moveResizingStartHeight) + deltaY
	case moveResizeLeft:
		//Move X coord and resize W
		xcoord += deltaW
		w = int16(x.moveResizingStartWidth) - deltaX
	case moveResizeMove, moveResizeMoveKeyboard:
		//Move both X,Y coords and no resize
		xcoord += deltaW
		ycoord += deltaH
	case moveResizeCancel:
		x.moveResizeEnd(c)
	}
	x.moveResizingLastX = moveX
	x.moveResizingLastY = moveY
	c.setWindowGeometry(xcoord, ycoord, uint16(w), uint16(h))
}

func (x *x11WM) handleMoveResize(ev xproto.ClientMessageEvent, c *client) {
	x.moveResizing = true
	x.moveResizingLastX = int16(ev.Data.Data32[0])
	x.moveResizingLastY = int16(ev.Data.Data32[1])
	x.moveResizingStartX = x.moveResizingLastX
	x.moveResizingStartY = x.moveResizingLastY
	_, _, x.moveResizingStartWidth, x.moveResizingStartHeight = c.getWindowGeometry()
	x.moveResizingType = moveResizeType(ev.Data.Data32[2])
	xproto.GrabPointer(x.x.Conn(), true, c.id,
		xproto.EventMaskButtonPress|xproto.EventMaskButtonRelease|xproto.EventMaskPointerMotion,
		xproto.GrabModeAsync, xproto.GrabModeAsync, x.x.RootWin(), xproto.CursorNone, xproto.TimeCurrentTime)
	xproto.GrabKeyboard(x.x.Conn(), true, c.id, xproto.TimeCurrentTime, xproto.GrabModeAsync, xproto.GrabModeAsync)
}

func (x *x11WM) handleStateActionRequest(ev xproto.ClientMessageEvent, removeState func(), addState func(), toggleCheck bool) {
	switch clientMessageStateAction(ev.Data.Data32[0]) {
	case clientMessageStateActionRemove:
		removeState()
	case clientMessageStateActionAdd:
		addState()
	case clientMessageStateActionToggle:
		if toggleCheck {
			removeState()
		} else {
			addState()
		}
	}
}

func (x *x11WM) handleVisibilityChange(ev xproto.VisibilityNotifyEvent) {
	attrs, err := xproto.GetWindowAttributes(x.x.Conn(), ev.Window).Reply()
	if err == nil && attrs.Colormap != 0 {
		if ev.State != xproto.VisibilityFullyObscured {
			xproto.InstallColormap(x.x.Conn(), attrs.Colormap)
		} else {
			xproto.UninstallColormap(x.x.Conn(), attrs.Colormap)
		}
	}
	colormaps, err := icccm.WmColormapWindowsGet(x.x, ev.Window)
	if err == nil {
		for _, child := range colormaps {
			chAttrs, err := xproto.GetWindowAttributes(x.x.Conn(), child).Reply()
			if err == nil && chAttrs.Colormap != 0 {
				if ev.State != xproto.VisibilityFullyObscured {
					xproto.InstallColormap(x.x.Conn(), chAttrs.Colormap)
				} else {
					xproto.UninstallColormap(x.x.Conn(), chAttrs.Colormap)
				}
			}
		}
	}
}

func (x *x11WM) handleInitialHints(ev xproto.ClientMessageEvent, hint string) {
	switch clientMessageStateAction(ev.Data.Data32[0]) {
	case clientMessageStateActionRemove:
		windowExtendedHintsRemove(x.x, ev.Window, hint)
	case clientMessageStateActionAdd:
		windowExtendedHintsAdd(x.x, ev.Window, hint)
	}
}

func (x *x11WM) handleClientMessage(ev xproto.ClientMessageEvent) {
	c := x.clientForWin(ev.Window)
	msgAtom, err := xprop.AtomName(x.x, ev.Type)
	if err != nil {
		fyne.LogError("Error getting event", err)
		return
	}
	switch msgAtom {
	case "WM_CHANGE_STATE":
		if c == nil {
			return
		}
		switch ev.Data.Bytes()[0] {
		case icccm.StateIconic:
			c.(*client).iconifyClient()
		case icccm.StateNormal:
			c.(*client).uniconifyClient()
		}
	case "_NET_ACTIVE_WINDOW":
		if c == nil {
			return
		}
		win, err := windowActiveGet(x.x)
		if err == nil && win == ev.Window {
			return
		}
		xproto.SetInputFocus(x.x.Conn(), 0, ev.Window, 0)
		windowActiveSet(x.x, ev.Window)
	case "_NET_WM_FULLSCREEN_MONITORS":
		// TODO WHEN WE SUPPORT MULTI-MONITORS - THIS TELLS WHICH/HOW MANY MONITORS
		// TO FULLSCREEN ACROSS
	case "_NET_WM_MOVERESIZE":
		if c == nil {
			return
		}
		if c.Maximized() || c.Fullscreened() {
			return
		}
		x.handleMoveResize(ev, c.(*client))
	case "_NET_WM_STATE":
		subMsgAtom, err := xprop.AtomName(x.x, xproto.Atom(ev.Data.Data32[1]))
		if err != nil {
			fyne.LogError("Error getting event", err)
			return
		}
		if c == nil {
			x.handleInitialHints(ev, subMsgAtom)
			return
		}
		switch subMsgAtom {
		case "_NET_WM_STATE_FULLSCREEN":
			x.handleStateActionRequest(ev, c.(*client).unfullscreenClient, c.(*client).fullscreenClient, c.Fullscreened())
		case "_NET_WM_STATE_HIDDEN":
			fyne.LogError("Extended Window Manager Hints says to ignore the HIDDEN state.", nil)
		//	x.handleStateActionRequest(ev, c.(*client).uniconifyClient, c.(*client).iconifyClient, c.Iconic())
		case "_NET_WM_STATE_MAXIMIZED_VERT", "_NET_WM_STATE_MAXIMIZED_HORZ":
			extraMsgAtom, err := xprop.AtomName(x.x, xproto.Atom(ev.Data.Data32[2]))
			if err != nil {
				fyne.LogError("Error getting event", err)
				return
			}
			if extraMsgAtom == subMsgAtom {
				return
			}
			if extraMsgAtom == "_NET_WM_STATE_MAXIMIZED_VERT" || extraMsgAtom == "_NET_WM_STATE_MAXIMIZED_HORZ" {
				x.handleStateActionRequest(ev, c.(*client).unmaximizeClient, c.(*client).maximizeClient, c.Maximized())
			}
		}
	}
}

func (x *x11WM) showWindow(win xproto.Window, parent xproto.Window) {
	name := windowName(x.x, win)

	if name == x.root.Title() {
		err := xproto.MapWindowChecked(x.x.Conn(), win).Check()
		if err != nil {
			fyne.LogError("Show Window Error", err)
		}
		x.bindKeys(win)
		go x.frameExisting()

		return
	}
	if x.rootID == 0 {
		return
	}
	hints, err := icccm.WmHintsGet(x.x, win)
	if err == nil {
		if hints.Flags&icccm.HintState > 0 && hints.InitialState == icccm.StateWithdrawn { // We don't want to manage windows that are not mapped
			return
		}
	}

	override := windowOverrideGet(x.x, win) // We don't want to manage windows that have an override on the WM
	if override {
		return
	}

	winType := windowTypeGet(x.x, win)
	switch winType[0] {
	case windowTypeUtility, windowTypeDialog, windowTypeNormal:
		break
	default:
		return
	}

	transient := windowTransientForGet(x.x, win)
	if transient > 0 && transient != win {
		x.transientChildAdd(transient, win)
	}

	x.setupWindow(win)
}

func (x *x11WM) hideWindow(win xproto.Window) {
	c := x.clientForWin(win)
	if c == nil {
		return
	}

	xproto.UnmapWindow(x.x.Conn(), c.(*client).id)
}

func (x *x11WM) setupWindow(win xproto.Window) {
	if windowName(x.x, win) == "" {
		windowExtendedHintsAdd(x.x, win, "_NET_WM_STATE_SKIP_TASKBAR")
		windowExtendedHintsAdd(x.x, win, "_NET_WM_STATE_SKIP_PAGER")
	}
	c := x.clientForWin(win)
	if c != nil {
		return
	}
	c = newClient(win, x)

	x.AddWindow(c)
	c.RaiseToTop()
	c.Focus()
	windowClientListUpdate(x)
	windowClientListStackingUpdate(x)
}

func (x *x11WM) destroyWindow(win xproto.Window) {
	c := x.clientForWin(win)
	if c == nil {
		return
	}

	transient := windowTransientForGet(x.x, win)
	if transient > 0 && transient != win {
		x.transientChildRemove(transient, win)
	} else if transient > 0 && transient == win {
		x.transientLeaderRemove(transient)
	}
	x.RemoveWindow(c)
	windowClientListUpdate(x)
	windowClientListStackingUpdate(x)
}

func (x *x11WM) bindKeys(win xproto.Window) {
	xproto.GrabKey(x.x.Conn(), true, win, xproto.ModMask1, keyCodeSpace, xproto.GrabModeAsync, xproto.GrabModeAsync)
	xproto.GrabKey(x.x.Conn(), true, win, xproto.ModMask1, keyCodeTab, xproto.GrabModeAsync, xproto.GrabModeAsync)
	xproto.GrabKey(x.x.Conn(), true, win, xproto.ModMaskShift|xproto.ModMask1, keyCodeTab, xproto.GrabModeAsync, xproto.GrabModeAsync)
}

func (x *x11WM) frameExisting() {
	tree, err := xproto.QueryTree(x.x.Conn(), x.x.RootWin()).Reply()
	if err != nil {
		fyne.LogError("Query Tree Error", err)
		return
	}

	for _, child := range tree.Children {
		name := windowName(x.x, child)
		if x.root != nil && name == x.root.Title() {
			continue
		}
		attrs, err := xproto.GetWindowAttributes(x.x.Conn(), child).Reply()
		if err != nil {
			fyne.LogError("Get Window Attributes Error", err)
			continue
		}
		if attrs.MapState == xproto.MapStateUnmapped {
			continue
		}
		x.setupWindow(child)
	}
}

func (x *x11WM) scaleToPixels(i int) uint16 {
	scale := float32(1.0)
	if x.root != nil {
		scale = x.root.Canvas().Scale()
	}

	return uint16(float32(i) * scale)
}

func (x *x11WM) takeSelectionOwnership() {
	name := fmt.Sprintf("WM_S%d", x.x.Conn().DefaultScreen)
	selAtom, err := xprop.Atm(x.x, name)
	if err != nil {
		fyne.LogError("Error getting selection atom", err)
		return
	}
	err = xproto.SetSelectionOwnerChecked(x.x.Conn(), x.x.Dummy(), selAtom, xproto.TimeCurrentTime).Check()
	if err != nil {
		fyne.LogError("Error setting selection owner", err)
		return
	}
	reply, err := xproto.GetSelectionOwner(x.x.Conn(), selAtom).Reply()
	if err != nil {
		fyne.LogError("Error getting selection owner", err)
		return
	}
	if reply.Owner != x.x.Dummy() {
		fyne.LogError("Could not obtain ownership - Another WM is likely running", err)
	}
	manAtom, err := xprop.Atm(x.x, "MANAGER")
	if err != nil {
		fyne.LogError("Error getting manager atom", err)
		return
	}
	cm, err := xevent.NewClientMessage(32, x.x.RootWin(), manAtom,
		xproto.TimeCurrentTime, int(selAtom), int(x.x.Dummy()))
	if err != nil {
		fyne.LogError("Error creating client message", err)
		return
	}
	xproto.SendEvent(x.x.Conn(), false, x.x.RootWin(), xproto.EventMaskStructureNotify,
		string(cm.Bytes()))
}
