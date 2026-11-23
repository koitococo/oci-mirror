package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func watchAuthFile(path string) (<-chan struct{}, func(), error) {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return nil, nil, fmt.Errorf("inotify init: %w", err)
	}
	file := os.NewFile(uintptr(fd), "upstream-authfile-inotify")
	// Watching the directory rather than the inode survives atomic replacement.
	const mask = unix.IN_CLOSE_WRITE | unix.IN_MOVED_TO | unix.IN_MOVED_FROM | unix.IN_DELETE | unix.IN_MOVE_SELF | unix.IN_DELETE_SELF | unix.IN_ONLYDIR
	if _, err := unix.InotifyAddWatch(fd, filepath.Dir(path), mask); err != nil {
		_ = file.Close()
		return nil, nil, fmt.Errorf("inotify watch parent directory: %w", err)
	}
	changes := make(chan struct{}, 1)
	go readAuthFileEvents(file, []byte(filepath.Base(path)), changes)
	return changes, func() { _ = file.Close() }, nil
}

func readAuthFileEvents(file *os.File, target []byte, changes chan<- struct{}) {
	defer close(changes)
	defer file.Close()
	var buf [4096]byte // Holds at least one inotify event with a NAME_MAX name.
	for {
		n, err := file.Read(buf[:])
		if err != nil {
			if !errors.Is(err, os.ErrClosed) {
				slog.Warn("upstream_authfile_watch_stopped", "error", err)
			}
			return
		}
		changed := false
		for events := buf[:n]; len(events) != 0; {
			// Linux inotify_event ABI: wd, mask, cookie, len, padded name.
			mask := binary.NativeEndian.Uint32(events[4:8])
			end := unix.SizeofInotifyEvent + int(binary.NativeEndian.Uint32(events[12:16]))
			name := bytes.TrimRight(events[unix.SizeofInotifyEvent:end], "\x00")
			events = events[end:]
			if mask&(unix.IN_DELETE_SELF|unix.IN_MOVE_SELF|unix.IN_IGNORED|unix.IN_UNMOUNT) != 0 {
				slog.Warn("upstream_authfile_watch_stopped", "reason", "directory_watch_lost")
				return
			}
			changed = changed || mask&unix.IN_Q_OVERFLOW != 0 || bytes.Equal(name, target)
		}
		if changed {
			select {
			case changes <- struct{}{}:
			default:
			}
		}
	}
}
