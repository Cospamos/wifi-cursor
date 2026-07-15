//go:build linux

package input

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const hiddenCursorThemeName = "wifi-cursor-hidden"

// waylandCursorHider swaps the desktop's cursor theme to a fully
// transparent one while forwarding, and restores the original theme
// afterwards.
//
// This exists specifically for Wayland sessions: XFixesHideCursor (used
// elsewhere in this file) is an X11-only mechanism and has no effect on the
// cursor a Wayland compositor actually draws, even through XWayland. There
// is no equivalent "just hide it" API on Wayland - swapping to a theme made
// of fully transparent cursor images is the standard workaround.
//
// Only GNOME (via gsettings) is wired up to reliably save and restore the
// original theme. KDE/Hyprland etc. aren't included: without a reliable way
// to read back "what was it before", a half-implementation risks leaving
// the cursor permanently invisible on that desktop instead - worse than not
// hiding it at all.
type waylandCursorHider struct {
	ready         bool
	originalTheme string
}

func newWaylandCursorHider() *waylandCursorHider {
	h := &waylandCursorHider{}
	if !isWaylandSession() {
		return h
	}
	if _, err := exec.LookPath("xcursorgen"); err != nil {
		return h // best-effort: no xcursorgen available, silently skip
	}
	if _, err := exec.LookPath("gsettings"); err != nil {
		return h
	}
	if err := buildTransparentCursorTheme(); err != nil {
		return h
	}
	h.originalTheme = gsettingsGet("org.gnome.desktop.interface", "cursor-theme")
	h.ready = true
	return h
}

func isWaylandSession() bool {
	return os.Getenv("WAYLAND_DISPLAY") != "" || os.Getenv("XDG_SESSION_TYPE") == "wayland"
}

func (h *waylandCursorHider) hide() {
	if !h.ready {
		return
	}
	gsettingsSet("org.gnome.desktop.interface", "cursor-theme", hiddenCursorThemeName)
}

func (h *waylandCursorHider) show() {
	if !h.ready || h.originalTheme == "" {
		return
	}
	gsettingsSet("org.gnome.desktop.interface", "cursor-theme", h.originalTheme)
}

func gsettingsGet(schema, key string) string {
	out, err := exec.Command("gsettings", "get", schema, key).Output()
	if err != nil {
		return ""
	}
	return strings.Trim(strings.TrimSpace(string(out)), "'")
}

func gsettingsSet(schema, key, value string) {
	_ = exec.Command("gsettings", "set", schema, key, value).Run()
}

// buildTransparentCursorTheme writes a minimal cursor theme, made of 1x1
// fully-transparent images, to the standard per-user icon theme location so
// GNOME/other XDG-icon-theme-aware shells can find it by name.
func buildTransparentCursorTheme() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".local", "share", "icons", hiddenCursorThemeName)
	cursorsDir := filepath.Join(dir, "cursors")
	if err := os.MkdirAll(cursorsDir, 0o755); err != nil {
		return err
	}

	indexTheme := "[Icon Theme]\nName=" + hiddenCursorThemeName + "\nComment=Fully transparent cursor (wifi-cursor)\n"
	if err := os.WriteFile(filepath.Join(dir, "index.theme"), []byte(indexTheme), 0o644); err != nil {
		return err
	}

	pngPath := filepath.Join(dir, "transparent.png")
	if err := writeTransparentPNG(pngPath); err != nil {
		return err
	}
	confPath := filepath.Join(dir, "cursor.conf")
	if err := os.WriteFile(confPath, []byte("1 0 0 transparent.png\n"), 0o644); err != nil {
		return err
	}

	// Covers the cursor names actually looked up in practice; xcursorgen
	// failing for any one of these (e.g. a name it doesn't like) is fine,
	// the rest still get created.
	names := []string{
		"left_ptr", "default", "pointer", "text", "xterm",
		"hand1", "hand2", "crosshair", "watch", "wait",
	}
	for _, name := range names {
		cmd := exec.Command("xcursorgen", confPath, filepath.Join(cursorsDir, name))
		cmd.Dir = dir
		_ = cmd.Run()
	}
	return nil
}

func writeTransparentPNG(path string) error {
	img := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.NRGBA{})
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}
