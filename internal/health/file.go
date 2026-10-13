package health

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

func processIdentity(pid int) (string, string, error) {
	if pid <= 0 {
		return "", "", errors.New("invalid listener PID")
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", "", err
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", "", err
	}
	end := strings.LastIndexByte(string(stat), ')')
	if end < 0 {
		return "", "", errors.New("invalid process identity")
	}
	fields := strings.Fields(string(stat[end+1:]))
	if len(fields) <= 19 || fields[0] == "Z" || fields[0] == "X" {
		return "", "", errors.New("listener process is not live")
	}
	if _, err := strconv.ParseUint(fields[19], 10, 64); err != nil {
		return "", "", err
	}
	return strings.TrimSpace(string(boot)), fields[19], nil
}

// Read rejects stale, foreign-process and insecure files instead of reporting
// their old observations as the live listener. expectedPID=0 permits any live
// process identity; service status supplies systemd's actual MainPID.
func Read(path string, now time.Time, expectedPID int) (Snapshot, error) {
	var s Snapshot
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return s, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close() // discard: read-only descriptor; no buffered output to lose.
	info, err := f.Stat()
	if err != nil {
		return s, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return s, errors.New("health snapshot must be a regular private 0600 file")
	}
	if err := json.NewDecoder(io.LimitReader(f, 64*1024)).Decode(&s); err != nil {
		return s, err
	}
	if s.Version != 1 || s.Updated.IsZero() || now.Sub(s.Updated) > FreshFor || s.Updated.After(now.Add(time.Second)) {
		return s, errors.New("listener snapshot is stale or invalid; liveness unverified")
	}
	if expectedPID != 0 && s.PID != expectedPID {
		return s, errors.New("snapshot PID does not match service MainPID")
	}
	boot, start, err := processIdentity(s.PID)
	if err != nil {
		return s, fmt.Errorf("listener identity unverified: %w", err)
	}
	if boot != s.BootID || start != s.ProcessStart {
		return s, errors.New("listener identity changed; snapshot belongs to an old process")
	}
	procInfo, err := os.Stat(fmt.Sprintf("/proc/%d", s.PID))
	if err != nil {
		return s, err
	}
	if info.Sys().(*syscall.Stat_t).Uid != procInfo.Sys().(*syscall.Stat_t).Uid {
		return s, errors.New("snapshot owner differs from listener owner")
	}
	return s, nil
}

type Publisher struct {
	path    string
	lock    *os.File
	monitor *Monitor
	boot    string
	start   string
	cancel  context.CancelFunc
	done    chan struct{}
	err     error
	once    sync.Once
}

// Publish takes a nonblocking lock for this state path. The lock inode stays
// in place on close: unlinking it would let two future publishers hold locks.
func Publish(ctx context.Context, path string, monitor *Monitor) (*Publisher, error) {
	boot, start, err := processIdentity(os.Getpid())
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	dir, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	if !dir.IsDir() || dir.Mode().Perm()&0022 != 0 || dir.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return nil, errors.New("health directory must be owned by listener and not writable by other accounts")
	}
	fd, err := syscall.Open(path+".lock", syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	lock := os.NewFile(uintptr(fd), path+".lock")
	info, err := lock.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return nil, errors.Join(errors.New("health publisher lock is not a private owned regular file"), lock.Close())
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, errors.Join(fmt.Errorf("another listener owns the health path: %w", err), lock.Close())
	}
	ctx, cancel := context.WithCancel(ctx)
	p := &Publisher{path: path, lock: lock, monitor: monitor, boot: boot, start: start, cancel: cancel, done: make(chan struct{})}
	if err := p.update(); err != nil {
		cancel()
		return nil, errors.Join(err, lock.Close())
	}
	go func() {
		defer close(p.done)
		ticker := time.NewTicker(PublishEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := p.update(); err != nil {
					p.err = err
					return
				}
			}
		}
	}()
	return p, nil
}

func (p *Publisher) update() (err error) {
	s := p.monitor.Snapshot(time.Now())
	s.PID, s.BootID, s.ProcessStart = os.Getpid(), p.boot, p.start
	f, err := os.CreateTemp(filepath.Dir(p.path), ".listener-health-*")
	if err != nil {
		return err
	}
	defer func() {
		removeErr := os.Remove(f.Name())
		if !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}()
	if err := json.NewEncoder(f).Encode(s); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), p.path)
}

func (p *Publisher) Close() error {
	p.once.Do(func() {
		p.cancel()
		<-p.done
		err := os.Remove(p.path)
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
		p.err = errors.Join(p.err, err, p.lock.Close())
	})
	return p.err
}
