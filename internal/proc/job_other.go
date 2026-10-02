//go:build !windows

package proc

import "errors"

// 本项目只面向 Windows。这个桩的存在只是为了让 `go vet ./...` 之类的
// 检查能在其他平台上跑通,真机上不会走到这里。

type Job struct{}

func NewJob() (*Job, error) {
	return nil, errors.New("proc: 仅支持 Windows")
}

func (j *Job) Assign(pid int) error { return errors.New("proc: 仅支持 Windows") }

func (j *Job) ActiveProcesses() (int, error) { return 0, errors.New("proc: 仅支持 Windows") }

func (j *Job) Close() {}
