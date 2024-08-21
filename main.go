// Copyright 2022 Ahmet Alp Balkan
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// TODO make it possible to "go generate" on Windows (https://github.com/josephspurrier/goversioninfo/issues/52).
//go:generate /bin/bash -c "go run github.com/josephspurrier/goversioninfo/cmd/goversioninfo@latest -arm -64 -icon=assets/icon.ico - <<< '{}'"

package main

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"reflect"
	"runtime"

	"github.com/getlantern/systray"
	"github.com/gonutz/w32/v2"

	"github.com/ahmetb/RectangleWin/w32ex"
)

var lastResized w32.HWND

func main() {
	runtime.LockOSThread() // since we bind hotkeys etc that need to dispatch their message here
	if !w32ex.SetProcessDPIAware() {
		panic("failed to set DPI aware")
	}

	autorun, err := AutoRunEnabled()
	if err != nil {
		panic(err)
	}
	fmt.Printf("autorun enabled=%v\n", autorun)
	printMonitors()

	edgeFuncs := [][]resizeFunc{
		{leftHalf, leftTwoThirds, leftOneThirds},
		{rightHalf, rightTwoThirds, rightOneThirds},
		{topHalf, topTwoThirds, topOneThirds},
		{bottomHalf, bottomTwoThirds, bottomOneThirds},
		{leftOneThirds, middleThirds, rightOneThirds},
	}
	edgeFuncTurn := make([]int, len(edgeFuncs))

	cycleFuncs := func(funcs [][]resizeFunc, turns *[]int, i int) {
		hwnd := w32.GetForegroundWindow()
		if hwnd == 0 {
			panic("foreground window is NULL")
		}
		if lastResized != hwnd {
			*turns = make([]int, len(edgeFuncs)) // reset
		}
		if _, err := resize(hwnd, funcs[i][(*turns)[i]%len(funcs[i])]); err != nil {
			fmt.Printf("warn: resize: %v\n", err)
			return
		}
		(*turns)[i]++
		for j := 0; j < len(*turns); j++ {
			if j != i {
				(*turns)[j] = 0
			}
		}
	}

	cycleEdgeFuncs := func(i int) { cycleFuncs(edgeFuncs, &edgeFuncTurn, i) }

	hks := []HotKey{
		{id: 1, mod: MOD_ALT | MOD_WIN | MOD_CONTROL | MOD_NOREPEAT, vk: w32ex.VK_N_S, callback: func() { cycleEdgeFuncs(0) }},
		{id: 2, mod: MOD_ALT | MOD_WIN | MOD_CONTROL | MOD_NOREPEAT, vk: w32ex.VK_N_F, callback: func() { cycleEdgeFuncs(1) }},
		{id: 3, mod: MOD_ALT | MOD_WIN | MOD_CONTROL | MOD_NOREPEAT, vk: w32ex.VK_N_E, callback: func() { cycleEdgeFuncs(2) }},
		{id: 4, mod: MOD_ALT | MOD_WIN | MOD_CONTROL | MOD_NOREPEAT, vk: w32ex.VK_N_D, callback: func() { cycleEdgeFuncs(3) }},
		{id: 50, mod: MOD_ALT | MOD_WIN, vk: w32.VK_SPACE, callback: func() {
			lastResized = 0 // cause edgeFuncTurn to be reset
			if err := maximize(); err != nil {
				fmt.Printf("warn: maximize: %v\n", err)
				return
			}
		}},
		{id: 51, mod: MOD_ALT | MOD_WIN, vk: w32.VK_BACK, callback: func() { cycleEdgeFuncs(4) }},
		{id: 52, mod: MOD_ALT | MOD_WIN, vk: w32.VK_DELETE, callback: func() {
			hwnd := w32.GetForegroundWindow()
			if hwnd == 0 {
				panic("foreground window is NULL")
			}
			if _, err := moveToNextMonitor(hwnd); err != nil {
				fmt.Printf("warn: maximize: %v\n", err)
				return
			}
		}},
	}

	var failedHotKeys []HotKey
	for _, hk := range hks {
		if !RegisterHotKey(hk) {
			failedHotKeys = append(failedHotKeys, hk)
		}
	}
	if len(failedHotKeys) > 0 {
		msg := "The following hotkey(s) are in use by another process:\n\n"
		for _, hk := range failedHotKeys {
			msg += "  - " + hk.Describe() + "\n"
		}
		msg += "\nTo use these hotkeys in RectangleWin, close the other process using the key combination(s)."
		showMessageBox(msg)
	}

	exitCh := make(chan os.Signal)
	signal.Notify(exitCh, os.Interrupt)
	go func() {
		<-exitCh
		fmt.Println("exit signal received")
		systray.Quit() // causes WM_CLOSE, WM_QUIT, not sure if a side-effect
	}()

	// TODO systray/systray.go already locks the OS thread in init()
	// however it's not clear if GetMessage(0,0) will continue to work
	// as we run "go initTray()" and not pin the thread that initializes the
	// tray.
	initTray()
	if err := msgLoop(); err != nil {
		panic(err)
	}
}

func showMessageBox(text string) {
	w32.MessageBox(w32.GetActiveWindow(), text, "RectangleWin", w32.MB_ICONWARNING|w32.MB_OK)
}
func modNeg(v, m int) int {
	return (v%m + m) % m
}
func moveToNextMonitor(hwnd w32.HWND) (bool, error) {
	if !isZonableWindow(hwnd) {
		fmt.Printf("warn: non-zonable window: %s\n", w32.GetWindowText(hwnd))
		return false, nil
	}
	rect := w32.GetWindowRect(hwnd)
	mon := w32.MonitorFromWindow(hwnd, w32.MONITOR_DEFAULTTONEAREST)
	hdc := w32.GetDC(hwnd)
	displayDPI := w32.GetDeviceCaps(hdc, w32.LOGPIXELSY)

	var monitors []w32.HMONITOR
	monitorIndex := 0
	curMonitorIndex := 0

	EnumMonitors(func(d w32.HMONITOR) bool {
		monitors = append(monitors, d)
		if d == mon {
			monitorIndex = curMonitorIndex
		}
		curMonitorIndex += 1
		return true
	})

	// move to monitor_index + 1
	mon = monitors[modNeg(monitorIndex-1, len(monitors))]

	if !w32.ReleaseDC(hwnd, hdc) {
		return false, fmt.Errorf("failed to ReleaseDC:%d", w32.GetLastError())
	}
	var monInfo w32.MONITORINFO
	if !w32.GetMonitorInfo(mon, &monInfo) {
		return false, fmt.Errorf("failed to GetMonitorInfo:%d", w32.GetLastError())
	}

	ok, frame := w32.DwmGetWindowAttributeEXTENDED_FRAME_BOUNDS(hwnd)
	if !ok {
		return false, fmt.Errorf("failed to DwmGetWindowAttributeEXTENDED_FRAME_BOUNDS:%d", w32.GetLastError())
	}
	windowDPI := w32ex.GetDpiForWindow(hwnd)
	resizedFrame := resizeForDpi(frame, int32(windowDPI), int32(displayDPI))

	fmt.Printf("> window: 0x%x %#v (w:%d,h:%d) mon=0x%X(@ display DPI:%d)\n", hwnd, rect, rect.Width(), rect.Height(), mon, displayDPI)
	fmt.Printf("> DWM frame:        %#v (W:%d,H:%d) @ window DPI=%v\n", frame, frame.Width(), frame.Height(), windowDPI)
	fmt.Printf("> DPI-less frame:   %#v (W:%d,H:%d)\n", resizedFrame, resizedFrame.Width(), resizedFrame.Height())

	// calculate how many extra pixels go to win10 invisible borders
	lExtra := resizedFrame.Left - rect.Left
	rExtra := -resizedFrame.Right + rect.Right
	tExtra := resizedFrame.Top - rect.Top
	bExtra := -resizedFrame.Bottom + rect.Bottom

	newPos := center(monInfo.RcWork, resizedFrame)

	// adjust offsets based on invisible borders
	newPos.Left -= lExtra
	newPos.Top -= tExtra
	newPos.Right += rExtra
	newPos.Bottom += bExtra

	lastResized = hwnd
	if sameRect(rect, &newPos) {
		fmt.Println("no resize")
		return false, nil
	}

	fmt.Printf("> resizing to: %#v (W:%d,H:%d)\n", newPos, newPos.Width(), newPos.Height())
	if !w32.ShowWindow(hwnd, w32.SW_SHOWNORMAL) { // normalize window first if it's set to SW_SHOWMAXIMIZE (and therefore stays maximized)
		return false, fmt.Errorf("failed to normalize window ShowWindow:%d", w32.GetLastError())
	}
	if !w32.SetWindowPos(hwnd, 0, int(newPos.Left), int(newPos.Top), int(newPos.Width()), int(newPos.Height()), w32.SWP_NOZORDER|w32.SWP_NOACTIVATE) {
		return false, fmt.Errorf("failed to SetWindowPos:%d", w32.GetLastError())
	}
	rect = w32.GetWindowRect(hwnd)
	fmt.Printf("> post-resize: %#v(W:%d,H:%d)\n", rect, rect.Width(), rect.Height())
	return true, nil
}

type resizeFunc func(disp, cur w32.RECT) w32.RECT

func center(disp, cur w32.RECT) w32.RECT {
	w := (disp.Width() - cur.Width()) / 2
	h := (disp.Height() - cur.Height()) / 2
	return w32.RECT{
		Left:   disp.Left + w,
		Right:  disp.Left + w + cur.Width(),
		Top:    disp.Top + h,
		Bottom: disp.Top + h + cur.Height()}
}

func resize(hwnd w32.HWND, f resizeFunc) (bool, error) {
	if !isZonableWindow(hwnd) {
		fmt.Printf("warn: non-zonable window: %s\n", w32.GetWindowText(hwnd))
		return false, nil
	}
	rect := w32.GetWindowRect(hwnd)
	mon := w32.MonitorFromWindow(hwnd, w32.MONITOR_DEFAULTTONEAREST)
	hdc := w32.GetDC(hwnd)
	displayDPI := w32.GetDeviceCaps(hdc, w32.LOGPIXELSY)
	if !w32.ReleaseDC(hwnd, hdc) {
		return false, fmt.Errorf("failed to ReleaseDC:%d", w32.GetLastError())
	}
	var monInfo w32.MONITORINFO
	if !w32.GetMonitorInfo(mon, &monInfo) {
		return false, fmt.Errorf("failed to GetMonitorInfo:%d", w32.GetLastError())
	}

	ok, frame := w32.DwmGetWindowAttributeEXTENDED_FRAME_BOUNDS(hwnd)
	if !ok {
		return false, fmt.Errorf("failed to DwmGetWindowAttributeEXTENDED_FRAME_BOUNDS:%d", w32.GetLastError())
	}
	windowDPI := w32ex.GetDpiForWindow(hwnd)
	resizedFrame := resizeForDpi(frame, int32(windowDPI), int32(displayDPI))

	fmt.Printf("> window: 0x%x %#v (w:%d,h:%d) mon=0x%X(@ display DPI:%d)\n", hwnd, rect, rect.Width(), rect.Height(), mon, displayDPI)
	fmt.Printf("> DWM frame:        %#v (W:%d,H:%d) @ window DPI=%v\n", frame, frame.Width(), frame.Height(), windowDPI)
	fmt.Printf("> DPI-less frame:   %#v (W:%d,H:%d)\n", resizedFrame, resizedFrame.Width(), resizedFrame.Height())

	// calculate how many extra pixels go to win10 invisible borders
	lExtra := resizedFrame.Left - rect.Left
	rExtra := -resizedFrame.Right + rect.Right
	tExtra := resizedFrame.Top - rect.Top
	bExtra := -resizedFrame.Bottom + rect.Bottom

	newPos := f(monInfo.RcWork, resizedFrame)

	// adjust offsets based on invisible borders
	newPos.Left -= lExtra
	newPos.Top -= tExtra
	newPos.Right += rExtra
	newPos.Bottom += bExtra

	lastResized = hwnd
	if sameRect(rect, &newPos) {
		fmt.Println("no resize")
		return false, nil
	}

	fmt.Printf("> resizing to: %#v (W:%d,H:%d)\n", newPos, newPos.Width(), newPos.Height())
	if !w32.ShowWindow(hwnd, w32.SW_SHOWNORMAL) { // normalize window first if it's set to SW_SHOWMAXIMIZE (and therefore stays maximized)
		return false, fmt.Errorf("failed to normalize window ShowWindow:%d", w32.GetLastError())
	}
	if !w32.SetWindowPos(hwnd, 0, int(newPos.Left), int(newPos.Top), int(newPos.Width()), int(newPos.Height()), w32.SWP_NOZORDER|w32.SWP_NOACTIVATE) {
		return false, fmt.Errorf("failed to SetWindowPos:%d", w32.GetLastError())
	}
	rect = w32.GetWindowRect(hwnd)
	fmt.Printf("> post-resize: %#v(W:%d,H:%d)\n", rect, rect.Width(), rect.Height())
	return true, nil
}

func maximize() error {
	hwnd := w32.GetForegroundWindow()
	if !isZonableWindow(hwnd) {
		return errors.New("foreground window is not zonable")
	}
	if !w32.ShowWindow(hwnd, w32.SW_MAXIMIZE) {
		return fmt.Errorf("failed to ShowWindow:%d", w32.GetLastError())
	}
	return nil
}

func resizeForDpi(src w32.RECT, from, to int32) w32.RECT {
	return w32.RECT{
		Left:   src.Left * to / from,
		Right:  src.Right * to / from,
		Top:    src.Top * to / from,
		Bottom: src.Bottom * to / from,
	}
}

func sameRect(a, b *w32.RECT) bool {
	return a != nil && b != nil && reflect.DeepEqual(*a, *b)
}
