// touch_proxy.go — coordinate-transforming evdev→uinput relay for rotated displays.
//
// Grabs the physical touch device exclusively and re-injects events through a
// virtual uinput device with transformed ABS_X/ABS_Y coordinates.  Cog only
// sees the virtual device, so no libinput/udev calibration is needed.
package main

import (
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// ioctl numbers for evdev and uinput (aarch64 / x86-64, 64-bit Linux).
const (
	eviocGrab    = uintptr(0x40044590) // _IOW('E', 0x90, int)
	eviocGAbsBase = uintptr(0x80184540) // EVIOCGABS(0); add abs code

	uiDevCreate  = uintptr(0x5501)
	uiDevDestroy = uintptr(0x5502)
	uiSetEvBit   = uintptr(0x40045564) // _IOW('U', 100, int)
	uiSetAbsBit  = uintptr(0x40045565)
	uiSetKeyBit  = uintptr(0x40045566)
	uiSetPropBit = uintptr(0x4004556e) // _IOW('U', 110, int)
)

const (
	evSyn = uint16(0x00)
	evKey = uint16(0x01)
	evAbs = uint16(0x03)

	absX      = uint16(0x00)
	absY      = uint16(0x01)
	absMtPosX = uint16(0x35)
	absMtPosY = uint16(0x36)

	btnTouch      = uint16(0x14a)
	btnToolFinger = uint16(0x145)

	inputPropDirect = 0x01
	busVirtual      = uint16(0x06)

	uinputMaxNameSize = 80
	absCnt            = 64

	// inputEventSize is sizeof(struct input_event) on 64-bit Linux.
	inputEventSize = 24

	proxyProbeInterval = 2 * time.Second
	proxyGrabTimeout   = 5 * time.Second
)

// inputEvent mirrors struct input_event (24 bytes on 64-bit Linux).
type inputEvent struct {
	Sec   int64
	Usec  int64
	Type  uint16
	Code  uint16
	Value int32
}

// absInfo mirrors struct input_absinfo.
type absInfo struct {
	Value      int32
	Minimum    int32
	Maximum    int32
	Fuzz       int32
	Flat       int32
	Resolution int32
}

// inputID mirrors struct input_id.
type inputID struct {
	Bustype uint16
	Vendor  uint16
	Product uint16
	Version uint16
}

// uinputUserDev mirrors struct uinput_user_dev (1116 bytes).
type uinputUserDev struct {
	Name         [uinputMaxNameSize]byte
	ID           inputID
	FFEffectsMax uint32
	Absmax       [absCnt]int32
	Absmin       [absCnt]int32
	Absfuzz      [absCnt]int32
	Absflat      [absCnt]int32
}

// rotationMatrix returns the 2×3 affine matrix for ROTATE_DISPLAY values.
// Matches the matrices used by LIBINPUT_CALIBRATION_MATRIX.
func rotationMatrix(rot string) (m [6]float64, ok bool) {
	switch strings.ToLower(rot) {
	case "inverted", "180":
		return [6]float64{-1, 0, 1, 0, -1, 1}, true
	case "left", "90":
		return [6]float64{0, 1, 0, -1, 0, 1}, true
	case "right", "270":
		return [6]float64{0, -1, 1, 1, 0, 0}, true
	}
	return [6]float64{}, false
}

// transformCoords applies the 2×3 matrix to absolute device coordinates.
func transformCoords(m [6]float64, x, y int32, xi, yi absInfo) (int32, int32) {
	xRange := math.Max(1, float64(xi.Maximum-xi.Minimum))
	yRange := math.Max(1, float64(yi.Maximum-yi.Minimum))
	xNorm := float64(x-xi.Minimum) / xRange
	yNorm := float64(y-yi.Minimum) / yRange

	xOutNorm := m[0]*xNorm + m[1]*yNorm + m[2]
	yOutNorm := m[3]*xNorm + m[4]*yNorm + m[5]

	rx := int32(math.Round(xOutNorm*xRange + float64(xi.Minimum)))
	ry := int32(math.Round(yOutNorm*yRange + float64(yi.Minimum)))

	if rx < xi.Minimum {
		rx = xi.Minimum
	} else if rx > xi.Maximum {
		rx = xi.Maximum
	}
	if ry < yi.Minimum {
		ry = yi.Minimum
	} else if ry > yi.Maximum {
		ry = yi.Maximum
	}
	return rx, ry
}

func ioctlInt(fd uintptr, req uintptr, val int) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(val))
	if errno != 0 {
		return errno
	}
	return nil
}

func getAbsInfo(fd uintptr, code uint16) (absInfo, error) {
	var info absInfo
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, eviocGAbsBase+uintptr(code), uintptr(unsafe.Pointer(&info)))
	if errno != 0 {
		return absInfo{}, fmt.Errorf("EVIOCGABS(%d): %w", code, errno)
	}
	return info, nil
}

func findDevicePath(pattern string) string {
	entries, _ := filepath.Glob("/sys/class/input/event*/device/name")
	for _, entry := range entries {
		data, err := os.ReadFile(entry)
		if err != nil {
			continue
		}
		name := strings.TrimSpace(string(data))
		if matched, _ := filepath.Match(pattern, name); matched {
			event := filepath.Base(filepath.Dir(filepath.Dir(entry)))
			return "/dev/input/" + event
		}
	}
	return ""
}

func readEvent(f *os.File) (inputEvent, error) {
	var ev inputEvent
	b := (*[inputEventSize]byte)(unsafe.Pointer(&ev))
	_, err := io.ReadFull(f, b[:])
	return ev, err
}

func writeRawEvent(fd int, ev inputEvent) {
	b := (*[inputEventSize]byte)(unsafe.Pointer(&ev))
	_, _ = syscall.Write(fd, b[:])
}

func writeEvent(fd int, typ, code uint16, value int32) {
	writeRawEvent(fd, inputEvent{Type: typ, Code: code, Value: value})
}

func createUinputDevice(xi, yi, mtxi, mtyi absInfo) (int, error) {
	fd, err := syscall.Open("/dev/uinput", syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return -1, fmt.Errorf("open /dev/uinput: %w", err)
	}
	ufd := uintptr(fd)

	for _, bit := range []int{int(evSyn), int(evKey), int(evAbs)} {
		if err := ioctlInt(ufd, uiSetEvBit, bit); err != nil {
			_ = syscall.Close(fd)
			return -1, fmt.Errorf("UI_SET_EVBIT(%d): %w", bit, err)
		}
	}
	for _, bit := range []uint16{btnTouch, btnToolFinger} {
		_ = ioctlInt(ufd, uiSetKeyBit, int(bit))
	}
	for _, bit := range []uint16{absX, absY, absMtPosX, absMtPosY} {
		_ = ioctlInt(ufd, uiSetAbsBit, int(bit))
	}
	_ = ioctlInt(ufd, uiSetPropBit, inputPropDirect)

	var dev uinputUserDev
	copy(dev.Name[:], "kiosk-touch-proxy")
	dev.ID = inputID{Bustype: busVirtual, Vendor: 0x1, Product: 0x1, Version: 1}
	dev.Absmax[absX] = xi.Maximum
	dev.Absmin[absX] = xi.Minimum
	dev.Absfuzz[absX] = xi.Fuzz
	dev.Absflat[absX] = xi.Flat
	dev.Absmax[absY] = yi.Maximum
	dev.Absmin[absY] = yi.Minimum
	dev.Absfuzz[absY] = yi.Fuzz
	dev.Absflat[absY] = yi.Flat
	dev.Absmax[absMtPosX] = mtxi.Maximum
	dev.Absmin[absMtPosX] = mtxi.Minimum
	dev.Absmax[absMtPosY] = mtyi.Maximum
	dev.Absmin[absMtPosY] = mtyi.Minimum

	b := (*[unsafe.Sizeof(dev)]byte)(unsafe.Pointer(&dev))
	if _, err := syscall.Write(fd, b[:]); err != nil {
		_ = syscall.Close(fd)
		return -1, fmt.Errorf("write uinput_user_dev: %w", err)
	}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, ufd, uiDevCreate, 0); errno != 0 {
		_ = syscall.Close(fd)
		return -1, fmt.Errorf("UI_DEV_CREATE: %w", errno)
	}
	return fd, nil
}

func relayLoop(src *os.File, ufd int, matrix [6]float64, xi, yi, mtxi, mtyi absInfo, stopCh <-chan struct{}) error {
	evCh := make(chan inputEvent, 64)
	errCh := make(chan error, 1)
	go func() {
		for {
			ev, err := readEvent(src)
			if err != nil {
				errCh <- err
				return
			}
			evCh <- ev
		}
	}()

	// Track pending position within each SYN_REPORT frame.
	px, py := xi.Minimum, yi.Minimum
	mpx, mpy := mtxi.Minimum, mtyi.Minimum
	xDirty, yDirty := false, false
	mxDirty, myDirty := false, false

	for {
		select {
		case <-stopCh:
			return nil
		case err := <-errCh:
			return err
		case ev := <-evCh:
			switch {
			case ev.Type == evAbs && ev.Code == absX:
				px, xDirty = ev.Value, true
			case ev.Type == evAbs && ev.Code == absY:
				py, yDirty = ev.Value, true
			case ev.Type == evAbs && ev.Code == absMtPosX:
				mpx, mxDirty = ev.Value, true
			case ev.Type == evAbs && ev.Code == absMtPosY:
				mpy, myDirty = ev.Value, true
			case ev.Type == evSyn:
				if xDirty || yDirty {
					tx, ty := transformCoords(matrix, px, py, xi, yi)
					writeEvent(ufd, evAbs, absX, tx)
					writeEvent(ufd, evAbs, absY, ty)
					xDirty, yDirty = false, false
				}
				if mxDirty || myDirty {
					tx, ty := transformCoords(matrix, mpx, mpy, mtxi, mtyi)
					writeEvent(ufd, evAbs, absMtPosX, tx)
					writeEvent(ufd, evAbs, absMtPosY, ty)
					mxDirty, myDirty = false, false
				}
				writeRawEvent(ufd, ev)
			default:
				writeRawEvent(ufd, ev)
			}
		}
	}
}

func proxyDevice(devPath string, matrix [6]float64, grabbed chan<- struct{}, stopCh <-chan struct{}) error {
	src, err := os.Open(devPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", devPath, err)
	}
	defer src.Close()
	fd := src.Fd()

	xi, err := getAbsInfo(fd, absX)
	if err != nil {
		return err
	}
	yi, err := getAbsInfo(fd, absY)
	if err != nil {
		return err
	}
	mtxi, err2 := getAbsInfo(fd, absMtPosX)
	if err2 != nil {
		mtxi = xi
	}
	mtyi, err2 := getAbsInfo(fd, absMtPosY)
	if err2 != nil {
		mtyi = yi
	}

	ufd, err := createUinputDevice(xi, yi, mtxi, mtyi)
	if err != nil {
		return err
	}
	defer func() {
		_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, uintptr(ufd), uiDevDestroy, 0)
		_ = syscall.Close(ufd)
	}()

	// Give the kernel a moment to expose the new virtual device via udev.
	time.Sleep(150 * time.Millisecond)

	if err := ioctlInt(fd, eviocGrab, 1); err != nil {
		return fmt.Errorf("EVIOCGRAB: %w", err)
	}
	defer func() { _ = ioctlInt(fd, eviocGrab, 0) }()

	log.Printf("touch-proxy: grabbed %s (ABS_X %d..%d, ABS_Y %d..%d)",
		devPath, xi.Minimum, xi.Maximum, yi.Minimum, yi.Maximum)
	grabbed <- struct{}{}

	return relayLoop(src, ufd, matrix, xi, yi, mtxi, mtyi, stopCh)
}

// runTouchProxy starts the evdev→uinput relay in the background.
// Signals grabbed once the physical device has been exclusively grabbed,
// or immediately if no proxy is needed (TOUCH_DEVICE or ROTATE_DISPLAY unset).
func runTouchProxy(grabbed chan<- struct{}, stopCh <-chan struct{}) {
	pattern := os.Getenv("TOUCH_DEVICE")
	rotation := os.Getenv("ROTATE_DISPLAY")
	matrix, ok := rotationMatrix(rotation)
	if pattern == "" || !ok {
		grabbed <- struct{}{}
		return
	}

	go func() {
		grabbedOnce := false
		for {
			devPath := findDevicePath(pattern)
			if devPath == "" {
				if !grabbedOnce {
					log.Printf("touch-proxy: device %q not found, retrying", pattern)
					grabbed <- struct{}{}
					grabbedOnce = true
				}
				select {
				case <-stopCh:
					return
				case <-time.After(proxyProbeInterval):
					continue
				}
			}

			log.Printf("touch-proxy: starting relay for %s (ROTATE_DISPLAY=%s)", devPath, rotation)
			g := make(chan struct{}, 1)
			if err := proxyDevice(devPath, matrix, g, stopCh); err != nil {
				log.Printf("touch-proxy: %v", err)
			}
			if !grabbedOnce {
				select {
				case <-g:
				default:
				}
				grabbed <- struct{}{}
				grabbedOnce = true
			}

			select {
			case <-stopCh:
				return
			case <-time.After(proxyProbeInterval):
			}
		}
	}()
}
