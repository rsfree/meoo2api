//go:build !unix

package store

import "os"

// acquireLock 在非 Unix 平台退化为无锁（单机单进程部署场景）。
func acquireLock(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
}

func releaseLock(f *os.File) error {
	if f == nil {
		return nil
	}
	return f.Close()
}
