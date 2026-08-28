//go:build !linux

package operations

import (
	"errors"
	"os/exec"
)

type HostLock struct{}

func AcquireHostLock(string) (*HostLock, error) {
	return nil, errors.New("operations candidate host locking is supported only on linux")
}

func (*HostLock) ConfigureChild(*exec.Cmd) (int, error) {
	return 0, errors.New("operations candidate host lock inheritance is supported only on linux")
}

func (*HostLock) Close() error { return nil }
