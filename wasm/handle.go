package wasm

import (
	"fmt"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/client/lib/fifo"
	"github.com/hashicorp/nomad/plugins/drivers"

	"huawei.com/wasm-task-driver/wasm/interfaces"
)

// taskHandle should store all relevant runtime information
// such as process ID if this is a local task or other meta
// data if this driver deals with external APIs.
type taskHandle struct {
	// stateLock syncs access to all fields below
	stateLock sync.RWMutex

	logger      hclog.Logger
	taskConfig  *drivers.TaskConfig
	procState   drivers.TaskState
	startedAt   time.Time
	completedAt time.Time
	exitResult  *drivers.ExitResult

	ioBufferConf IOBufferConfig
	mainFunc     Main
	instance     interfaces.WasmInstance
	completionCh chan struct{}
}

func (h *taskHandle) TaskStatus() *drivers.TaskStatus {
	h.stateLock.RLock()
	defer h.stateLock.RUnlock()

	return &drivers.TaskStatus{
		ID:          h.taskConfig.ID,
		Name:        h.taskConfig.Name,
		State:       h.procState,
		StartedAt:   h.startedAt,
		CompletedAt: h.completedAt,
		ExitResult:  h.exitResult,
	}
}

func (h *taskHandle) IsRunning() bool {
	h.stateLock.RLock()
	defer h.stateLock.RUnlock()

	return h.procState == drivers.TaskStateRunning
}

func (h *taskHandle) run() {
	defer close(h.completionCh)
	defer h.instance.Cleanup()

	h.stateLock.Lock()
	if h.exitResult == nil {
		h.exitResult = &drivers.ExitResult{}
	}
	h.stateLock.Unlock()

	var ioBuffer []byte

	if h.ioBufferConf.Enabled {
		inputByte := []byte(h.ioBufferConf.InputValue)
		if len(inputByte) > int(h.ioBufferConf.Size) {
			h.reportError(fmt.Errorf("input must be less than or equal to %d bytes to fit IO buffer", h.ioBufferConf.Size))

			return
		}

		h.ioBufferConf.Args = append([]int32{h.ioBufferConf.Size}, h.ioBufferConf.Args...)

		ptr, err := h.instance.CallFunc(h.ioBufferConf.IOBufFuncName, intListToIfaceList(h.ioBufferConf.Args)...)
		if err != nil {
			h.reportError(fmt.Errorf("unable to call %s function: %w", h.ioBufferConf.IOBufFuncName, err))

			return
		}

		offset := ptr.(int32)

		ioBuffer, err = h.instance.GetMemoryRange(offset, h.ioBufferConf.Size)
		if err != nil {
			h.reportError(fmt.Errorf("unable to get memory: %w", err))

			return
		}

		n := copy(ioBuffer, inputByte)

		h.logger.Debug("copied data from task config to IO buffer", "bytes", n)

		//nolint:gosec
		h.mainFunc.Args = append([]int32{offset, int32(n)}, h.mainFunc.Args...)
	}

	result, err := h.instance.CallFunc(h.mainFunc.MainFuncName, intListToIfaceList(h.mainFunc.Args)...)
	if err != nil {
		h.reportError(fmt.Errorf("failed to call %s: %w", h.mainFunc.MainFuncName, err))

		return
	}

	var out []byte

	if h.ioBufferConf.Enabled {
		resultSize := result.(int32)
		if resultSize == 0 {
			h.reportError(fmt.Errorf("unsuccessful WASM call"))

			return
		}

		out = make([]byte, resultSize)
		_ = copy(out, ioBuffer[:resultSize])
	} else {
		out = []byte(fmt.Sprintf("%v", result))
	}

	stdio, err := fifo.OpenWriter(h.taskConfig.StdoutPath)
	if err != nil {
		h.reportError(err)

		return
	}
	defer stdio.Close()

	n, err := stdio.Write(out)
	if err != nil {
		h.reportError(err)

		return
	}

	h.logger.Debug("wrote data to stdout", "bytes", n)

	h.reportCompletion()
}

func (h *taskHandle) reportError(err error) {
	h.stateLock.Lock()
	defer h.stateLock.Unlock()

	h.exitResult.Err = err
	h.procState = drivers.TaskStateUnknown
	h.completedAt = time.Now()
}

func (h *taskHandle) reportCompletion() {
	h.stateLock.Lock()
	defer h.stateLock.Unlock()

	h.procState = drivers.TaskStateExited
	h.exitResult.ExitCode = 0
	h.exitResult.Signal = 0
	h.completedAt = time.Now()
}

func intListToIfaceList(input []int32) []interface{} {
	result := make([]interface{}, len(input))

	for i := range input {
		result[i] = input[i]
	}

	return result
}
