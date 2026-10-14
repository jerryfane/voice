package device

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jerryfane/voice/internal/config"
	"github.com/jerryfane/voice/internal/proc"
)

const androidUITimeout = 5 * time.Second

// AndroidObservation is tool output, never a spoken device receipt. The PNG is
// the full screencap, not a rendered hierarchy. Each observation replaces the
// previous local screenshot; callers should inspect it before the next observe.
type AndroidObservation struct {
	ScreenshotPath string   `json:"screenshot_path"`
	UIHierarchy    string   `json:"ui_hierarchy"`
	Warnings       []string `json:"warnings,omitempty"`
}

func (d *Android) checkAgentControl() error {
	if d.err != nil {
		return d.err
	}
	if !d.agentControl {
		return errors.New("general Android control requires android.agent_control=true")
	}
	return nil
}

// ADB forwards argv to the configured phone, without a host shell. Arguments
// after 'shell' intentionally have Android's remote-shell semantics. Output may
// contain private data and must remain on the tool channel, including failures.
func (d *Android) ADB(ctx context.Context, args []string) (proc.Result, error) {
	if err := d.checkAgentControl(); err != nil {
		return proc.Result{}, err
	}
	if err := androidAgentArgs(args); err != nil {
		return proc.Result{}, err
	}
	return d.agentRun(ctx, args...)
}

func androidAgentArgs(args []string) error {
	if len(args) == 0 || args[0] == "" || strings.HasPrefix(args[0], "-") {
		return errors.New("provide an ADB command, not global options; the configured serial and host transport are fixed")
	}
	// These are host/server transport management, not phone actions. In
	// particular reconnect can operate on all devices despite a supplied -s.
	switch args[0] {
	case "connect", "disconnect", "pair", "reconnect", "attach", "detach", "start-server", "kill-server", "server", "nodaemon", "fork-server":
		return errors.New("ADB host transport management is not available through phone control")
	}
	// ADB dispatches commands following wait-for-* recursively. Keep waits
	// standalone so another command cannot sneak in global transport options.
	if strings.HasPrefix(args[0], "wait-for-") && len(args) != 1 {
		return errors.New("ADB wait-for commands must be standalone; issue the next phone command separately")
	}
	for _, arg := range args {
		if strings.IndexByte(arg, 0) >= 0 {
			return errors.New("ADB arguments cannot contain NUL bytes")
		}
	}
	return nil
}

func (d *Android) agentRun(ctx context.Context, args ...string) (proc.Result, error) {
	if err := ctx.Err(); err != nil {
		return proc.Result{}, err
	}
	argv := make([]string, 0, 3+len(args))
	argv = append(argv, "adb", "-s", d.serial)
	argv = append(argv, args...)
	out, err := proc.Run(ctx, argv, nil, 0)
	if ctx.Err() != nil {
		return out, ctx.Err()
	}
	return out, err
}

// Observe captures the actual screen before attempting UI XML. UI automation
// is not supported on every screen; its bounded failure becomes an explicit
// warning, while a failed screenshot or caller cancellation remains an error.
func (d *Android) Observe(ctx context.Context) (observation AndroidObservation, err error) {
	if err := d.checkAgentControl(); err != nil {
		return AndroidObservation{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	dir, err := config.StatePath("android")
	if err != nil {
		return AndroidObservation{}, err
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return AndroidObservation{}, err
	}
	unlock, err := androidObservationLock(ctx, dir)
	if err != nil {
		return AndroidObservation{}, fmt.Errorf("prepare private Android observation: %w", err)
	}
	defer func() {
		if closeErr := unlock(); closeErr != nil {
			observation = AndroidObservation{}
			err = errors.Join(err, fmt.Errorf("close Android observation lock: %w", closeErr))
		}
	}()

	captureCtx, captureCancel := context.WithTimeout(ctx, androidTimeout)
	out, err := d.agentRun(captureCtx, "exec-out", "screencap", "-p")
	captureCancel()
	if err != nil {
		return AndroidObservation{}, fmt.Errorf("capture Android screenshot: %w", err)
	}
	if err := androidScreenshot(out.Stdout); err != nil {
		return AndroidObservation{}, err
	}
	if err := ctx.Err(); err != nil {
		return AndroidObservation{}, err
	}
	path := filepath.Join(dir, "screen.png")
	if err := androidSaveScreenshot(path, out.Stdout); err != nil {
		return AndroidObservation{}, fmt.Errorf("store private Android screenshot: %w", err)
	}
	observation = AndroidObservation{ScreenshotPath: path}
	uiCtx, uiCancel := context.WithTimeout(ctx, androidUITimeout)
	defer uiCancel()
	// A fixed remote temporary file is necessary on phones where /dev/tty is
	// unavailable under exec-out. Remove it before dumping, so stale XML can
	// never masquerade as this observation, and clean it on normal exit.
	out, err = d.agentRun(uiCtx, "shell", "-T", "sh", "-c", "'p=/data/local/tmp/voice-window.xml; trap \"rm -f $p\" EXIT; rm -f \"$p\" && uiautomator dump \"$p\" >/dev/null && cat \"$p\"'")
	if ctx.Err() != nil {
		return AndroidObservation{}, ctx.Err()
	}
	if err == nil {
		observation.UIHierarchy, err = androidHierarchy(out.Stdout)
	}
	if err != nil {
		observation.Warnings = []string{fmt.Sprintf("UI hierarchy unavailable: %v; inspect the screenshot visually", err)}
	}
	return observation, nil
}

func androidScreenshot(data []byte) error {
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("Android screencap did not return a PNG: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > 64*1024*1024 {
		return errors.New("Android screencap dimensions exceed the 64-megapixel capture limit")
	}
	// Decode the whole image: a valid header with truncated/corrupt pixels is
	// not a successful screenshot. Preserve the original full PNG on disk.
	if _, err := png.Decode(bytes.NewReader(data)); err != nil {
		return fmt.Errorf("Android screencap returned a corrupt PNG: %w", err)
	}
	return nil
}

func androidHierarchy(data []byte) (string, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	depth, roots := 0, 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", errors.New("uiautomator returned invalid XML")
		}
		switch token := token.(type) {
		case xml.StartElement:
			if depth == 0 {
				roots++
				if token.Name.Local != "hierarchy" || roots != 1 {
					return "", errors.New("uiautomator returned no unique hierarchy root")
				}
			}
			depth++
		case xml.EndElement:
			depth--
		case xml.CharData:
			if depth == 0 && strings.TrimSpace(string(token)) != "" {
				return "", errors.New("uiautomator returned non-XML output")
			}
		}
	}
	if roots != 1 || depth != 0 {
		return "", errors.New("uiautomator returned no complete hierarchy")
	}
	return string(data), nil
}

func androidObservationLock(ctx context.Context, dir string) (func() error, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("Android state path must be a directory, not a symlink")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "observe.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lock.Chmod(0o600); err != nil {
		return nil, errors.Join(err, lock.Close())
	}
	var ticker *time.Ticker
	defer func() {
		if ticker != nil {
			ticker.Stop()
		}
	}()
	for {
		err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return lock.Close, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return nil, errors.Join(err, lock.Close())
		}
		if ticker == nil {
			ticker = time.NewTicker(25 * time.Millisecond)
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(ctx.Err(), lock.Close())
		case <-ticker.C:
		}
	}
}

func androidSaveScreenshot(path string, data []byte) (err error) {
	// One fixed staging file under the cross-process lock keeps crashes and
	// repeated observations bounded. O_NOFOLLOW rejects pre-existing symlinks.
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if removeErr := os.Remove(tmp); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("remove Android screenshot staging file: %w", removeErr))
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return errors.Join(err, f.Close())
	}
	_, writeErr := f.Write(data)
	if err := errors.Join(writeErr, f.Close()); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
