package device

import (
	"bytes"
	"context"
	"encoding/json"
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
	"unicode/utf8"

	"github.com/jerryfane/voice/internal/config"
	"github.com/jerryfane/voice/internal/proc"
)

const (
	androidUITimeout     = 5 * time.Second
	androidUIMaxElements = 128
	androidUIMaxBytes    = 32 * 1024
)

// AndroidObserveMode selects compact UI, a screenshot without UI automation,
// or both. UI failure automatically falls back to a fresh screenshot.
type AndroidObserveMode uint8

const (
	AndroidObserveUI AndroidObserveMode = iota
	AndroidObserveScreenshot
	AndroidObserveBoth
)

// AndroidObservation is private tool output, never a spoken device receipt.
// ScreenshotPath is present only for a fresh full-screen capture. Its private
// file is replaced or removed by the next observation.
type AndroidObservation struct {
	Kind           string     `json:"kind"`
	UI             *AndroidUI `json:"ui,omitempty"`
	ScreenshotPath string     `json:"screenshot_path,omitempty"`
	Warnings       []string   `json:"warnings,omitempty"`
}

// AndroidUI omits decorative containers and bounds the serialized elements.
// Truncated includes shortened fields; OmittedElements counts excluded useful
// nodes. General ADB access remains available for full hierarchy recovery.
type AndroidUI struct {
	Elements        []AndroidUIElement `json:"elements"`
	Truncated       bool               `json:"truncated,omitempty"`
	OmittedElements int                `json:"omitted_elements,omitempty"`
}

// AndroidUIElement retains labels and the nearest retained ancestor target.
// IDs are observation-local. States lists true flags; absent flags are false.
// Enabled, checked and selected therefore preserve their negative states too.
type AndroidUIElement struct {
	ID              int      `json:"id"`
	ParentID        int      `json:"parent_id,omitempty"`
	Text            string   `json:"text,omitempty"`
	Description     string   `json:"description,omitempty"`
	Role            string   `json:"role,omitempty"`
	ResourceID      string   `json:"resource_id,omitempty"`
	Bounds          string   `json:"bounds,omitempty"`
	States          []string `json:"states"`
	TruncatedFields []string `json:"truncated_fields,omitempty"`
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

// Observe returns compact actionable UI by default. Missing, invalid or timed
// out UI triggers a fresh PNG; screenshot-only mode never starts UI automation.
func (d *Android) Observe(ctx context.Context, mode AndroidObserveMode) (observation AndroidObservation, err error) {
	if err := d.checkAgentControl(); err != nil {
		return AndroidObservation{}, err
	}
	if mode > AndroidObserveBoth {
		return AndroidObservation{}, errors.New("invalid Android observation mode")
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
	// Invalidate the previous image even for UI-only observations or failures.
	// No old screenshot may masquerade as the current screen.
	path := filepath.Join(dir, "screen.png")
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return AndroidObservation{}, fmt.Errorf("remove previous Android screenshot: %w", err)
	}
	observation = AndroidObservation{Kind: "voice.android.observation"}
	if mode != AndroidObserveScreenshot {
		uiCtx, uiCancel := context.WithTimeout(ctx, androidUITimeout)
		// Remove the fixed remote file before dumping so stale XML cannot
		// masquerade as the current screen; clean it on normal exit.
		out, uiErr := d.agentRun(uiCtx, "shell", "-T", "sh", "-c", "'p=/data/local/tmp/voice-window.xml; trap \"rm -f $p\" EXIT; rm -f \"$p\" && uiautomator dump \"$p\" >/dev/null && cat \"$p\"'")
		uiCancel()
		if ctx.Err() != nil {
			return AndroidObservation{}, ctx.Err()
		}
		if uiErr == nil {
			observation.UI, uiErr = androidHierarchy(out.Stdout)
		}
		if ctx.Err() != nil {
			return AndroidObservation{}, ctx.Err()
		}
		if uiErr != nil {
			observation.Warnings = []string{fmt.Sprintf("UI hierarchy unavailable: %v; inspect the fresh screenshot visually", uiErr)}
		} else if mode == AndroidObserveUI {
			return observation, nil
		}
	}

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
	if err := androidSaveScreenshot(path, out.Stdout); err != nil {
		return AndroidObservation{}, fmt.Errorf("store private Android screenshot: %w", err)
	}
	observation.ScreenshotPath = path
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

func androidHierarchy(data []byte) (*AndroidUI, error) {
	if len(data) > 2*1024*1024 {
		return nil, errors.New("UI hierarchy exceeds the 2 MiB parsing limit")
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	ui := &AndroidUI{Elements: make([]AndroidUIElement, 0, 32)}
	parents := make([]int, 0, 32)
	roots, nextID, size := 0, 0, 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errors.New("uiautomator returned invalid XML")
		}
		switch token := token.(type) {
		case xml.StartElement:
			if len(parents) == 0 {
				roots++
				if token.Name.Local != "hierarchy" || roots != 1 {
					return nil, errors.New("uiautomator returned no unique hierarchy root")
				}
				parents = append(parents, 0)
				continue
			}
			if token.Name.Local != "node" || len(parents) >= 256 {
				return nil, errors.New("uiautomator returned an unsupported hierarchy structure")
			}
			parent := parents[len(parents)-1]
			element, useful := androidUIElement(token.Attr)
			if useful {
				nextID++
				element.ID, element.ParentID = nextID, parent
				// Once a node is omitted, keep a prefix so descendants never
				// point at missing ancestors. Continue parsing to validate all
				// XML and count the useful nodes explicitly excluded.
				if ui.OmittedElements > 0 || len(ui.Elements) >= androidUIMaxElements {
					ui.OmittedElements++
					ui.Truncated = true
				} else {
					encoded, err := json.Marshal(element)
					if err != nil {
						return nil, fmt.Errorf("encode UI element: %w", err)
					}
					if size+len(encoded)+1 > androidUIMaxBytes {
						ui.OmittedElements++
						ui.Truncated = true
					} else {
						size += len(encoded) + 1
						ui.Elements = append(ui.Elements, element)
						parent = element.ID
						ui.Truncated = ui.Truncated || len(element.TruncatedFields) > 0
					}
				}
			}
			parents = append(parents, parent)
		case xml.EndElement:
			parents = parents[:len(parents)-1]
		case xml.CharData:
			if strings.TrimSpace(string(token)) != "" {
				return nil, errors.New("uiautomator returned non-hierarchy text")
			}
		}
	}
	if roots != 1 || len(parents) != 0 || len(ui.Elements) == 0 {
		return nil, errors.New("uiautomator returned no usable hierarchy")
	}
	return ui, nil
}

func androidUIElement(attrs []xml.Attr) (AndroidUIElement, bool) {
	var element AndroidUIElement
	useful, editable, explicitEditable := false, false, false
	for _, attr := range attrs {
		value := attr.Value
		switch attr.Name.Local {
		case "text":
			element.Text = value
			useful = useful || strings.TrimSpace(value) != ""
		case "content-desc":
			element.Description = value
			useful = useful || strings.TrimSpace(value) != ""
		case "class":
			element.Role = value[strings.LastIndexByte(value, '.')+1:]
			editable = strings.HasSuffix(value, "EditText")
		case "resource-id":
			element.ResourceID = value
		case "bounds":
			element.Bounds = value
		case "clickable", "long-clickable", "editable", "scrollable", "enabled", "checkable", "checked", "selected", "password", "focusable", "focused":
			explicitEditable = explicitEditable || attr.Name.Local == "editable"
			if value == "true" {
				state := strings.ReplaceAll(attr.Name.Local, "-", "_")
				element.States = append(element.States, state)
				useful = useful || state != "enabled"
			}
		}
	}
	if editable && !explicitEditable {
		element.States = append(element.States, "editable")
		useful = true
	}
	if !useful {
		return element, false
	}
	if element.States == nil {
		element.States = []string{}
	}
	androidUIField(&element.Text, "text", 512, &element.TruncatedFields)
	androidUIField(&element.Description, "description", 512, &element.TruncatedFields)
	androidUIField(&element.Role, "role", 128, &element.TruncatedFields)
	androidUIField(&element.ResourceID, "resource_id", 256, &element.TruncatedFields)
	androidUIField(&element.Bounds, "bounds", 128, &element.TruncatedFields)
	return element, true
}

func androidUIField(value *string, name string, limit int, truncated *[]string) {
	if len(*value) <= limit {
		return
	}
	for !utf8.RuneStart((*value)[limit]) {
		limit--
	}
	*value = (*value)[:limit]
	*truncated = append(*truncated, name)
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
