//go:build unix

package store

import (
	"fmt"
	"os"
	"syscall"
)

// acquireLock 用 flock 保证同一数据目录只有一个写进程。
func acquireLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("%w: %s", ErrLocked, path)
	}
	_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
	return f, nil
}

func releaseLock(f *os.File) error {
	if f == nil {
		return nil
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return f.Close()
}
