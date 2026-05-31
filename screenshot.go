package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"strconv"
	"strings"
)

const framebufferRoot = "/sys/class/graphics/fb0"

// captureScreenshot reads the Linux framebuffer (/dev/fb0) and returns a PNG.
// Requires the DRM driver to expose a legacy framebuffer. Returns an error when
// /dev/fb0 is not available so callers can return 503 instead of panicking.
func captureScreenshot() ([]byte, error) {
	width, height, err := readFramebufferSize()
	if err != nil {
		return nil, err
	}

	bpp, err := readFramebufferInt("bits_per_pixel")
	if err != nil {
		return nil, err
	}
	bytesPerPixel := bpp / 8
	if bpp%8 != 0 || bytesPerPixel < 2 || bytesPerPixel > 4 {
		return nil, fmt.Errorf("unsupported framebuffer bit depth: %d", bpp)
	}

	stride, err := readFramebufferStride(width, bytesPerPixel)
	if err != nil {
		return nil, err
	}
	if stride < width*bytesPerPixel {
		return nil, fmt.Errorf("framebuffer stride %d is smaller than visible row %d", stride, width*bytesPerPixel)
	}

	f, err := os.Open("/dev/fb0") //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("cannot open framebuffer: %w", err)
	}
	defer f.Close()

	raw := make([]byte, height*stride)
	if _, err := io.ReadFull(f, raw); err != nil {
		return nil, fmt.Errorf("cannot read framebuffer (%dx%d, %dbpp, stride %d): %w", width, height, bpp, stride, err)
	}

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			i := y*stride + x*bytesPerPixel
			img.SetRGBA(x, y, framebufferPixel(raw[i:i+bytesPerPixel], bpp))
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("png encode failed: %w", err)
	}
	return buf.Bytes(), nil
}

func readFramebufferSize() (int, int, error) {
	sizeRaw, err := os.ReadFile(framebufferRoot + "/virtual_size")
	if err != nil {
		return 0, 0, fmt.Errorf("framebuffer not available: %w", err)
	}
	parts := strings.SplitN(strings.TrimSpace(string(sizeRaw)), ",", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("unexpected virtual_size format: %q", strings.TrimSpace(string(sizeRaw)))
	}
	width, err1 := strconv.Atoi(parts[0])
	height, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || width <= 0 || height <= 0 {
		return 0, 0, fmt.Errorf("cannot parse framebuffer dimensions from %q", strings.TrimSpace(string(sizeRaw)))
	}
	return width, height, nil
}

func readFramebufferInt(name string) (int, error) {
	raw, err := os.ReadFile(framebufferRoot + "/" + name)
	if err != nil {
		return 0, fmt.Errorf("cannot read framebuffer %s: %w", name, err)
	}
	value, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("cannot parse framebuffer %s from %q", name, strings.TrimSpace(string(raw)))
	}
	return value, nil
}

func readFramebufferStride(width, bytesPerPixel int) (int, error) {
	for _, name := range []string{"stride", "line_length"} {
		value, err := readFramebufferInt(name)
		if err == nil {
			return value, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return 0, err
		}
	}
	return width * bytesPerPixel, nil
}

func framebufferPixel(raw []byte, bpp int) color.RGBA {
	switch bpp {
	case 16:
		v := binary.LittleEndian.Uint16(raw)
		r := uint8(((v >> 11) & 0x1f) * 255 / 31)
		g := uint8(((v >> 5) & 0x3f) * 255 / 63)
		b := uint8((v & 0x1f) * 255 / 31)
		return color.RGBA{R: r, G: g, B: b, A: 255}
	case 24:
		return color.RGBA{R: raw[2], G: raw[1], B: raw[0], A: 255}
	default:
		return color.RGBA{R: raw[2], G: raw[1], B: raw[0], A: raw[3]}
	}
}
