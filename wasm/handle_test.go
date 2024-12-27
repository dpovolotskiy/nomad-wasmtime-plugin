package wasm

import (
	"os"
	"testing"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
)

func init() {
	hclog.Default().SetLevel(hclog.Debug)
}

var (
	IOBufValidSize int32 = 20

	IOBufValidInput = "this is normal input"

	failedFuncCallErr     = errors.New("function call failed")
	failedGetMemErr       = errors.New("failed to get memory range")
	unexpectedFuncNameErr = errors.New("unexpected function name")
)

type mockInstance struct{}

func (i *mockInstance) CallFunc(funcName string, _args ...interface{}) (interface{}, error) {
	switch funcName {
	case "failed":
		return nil, failedFuncCallErr
	case "failedMemoryGet":
		return int32(-1), nil
	case "unsuccessWASMExecution", "workingIOBufFunc":
		return int32(0), nil
	case "workingMainFunc":
		return IOBufValidSize, nil
	default:
		return nil, unexpectedFuncNameErr
	}
}

func (i *mockInstance) GetMemoryRange(start int32, size int32) ([]byte, error) {
	if start < 0 || size < 0 {
		return nil, failedGetMemErr
	}

	return make([]byte, size), nil
}

func (i *mockInstance) Stop() {}

func (i *mockInstance) Cleanup() {}

func TestRun(t *testing.T) {
	tCases := []struct {
		name              string
		tHandle           *taskHandle
		expectedErrMsg    string
		expectedTaskState drivers.TaskState
		stdOutPathPresent bool
	}{
		{
			name: "input is more than IO buffer size",
			tHandle: &taskHandle{
				completionCh: make(chan struct{}),
				instance:     &mockInstance{},
				ioBufferConf: IOBufferConfig{
					Enabled:    true,
					Size:       1,
					InputValue: "more than one byte",
				},
			},
			expectedErrMsg:    "input must be less than or equal to 1 bytes to fit IO buffer",
			expectedTaskState: drivers.TaskStateUnknown,
		},
		{
			name: "failed to call IOBufferFunc",
			tHandle: &taskHandle{
				completionCh: make(chan struct{}),
				instance:     &mockInstance{},
				ioBufferConf: IOBufferConfig{
					Enabled:       true,
					Size:          IOBufValidSize,
					InputValue:    IOBufValidInput,
					IOBufFuncName: "failed",
				},
			},
			expectedErrMsg:    "unable to call failed function: function call failed",
			expectedTaskState: drivers.TaskStateUnknown,
		},
		{
			name: "failed to get memory",
			tHandle: &taskHandle{
				completionCh: make(chan struct{}),
				instance:     &mockInstance{},
				ioBufferConf: IOBufferConfig{
					Enabled:       true,
					Size:          IOBufValidSize,
					InputValue:    IOBufValidInput,
					IOBufFuncName: "failedMemoryGet",
				},
			},
			expectedErrMsg:    "unable to get memory: failed to get memory range",
			expectedTaskState: drivers.TaskStateUnknown,
		},
		{
			name: "failed to call MainFunc",
			tHandle: &taskHandle{
				completionCh: make(chan struct{}),
				instance:     &mockInstance{},
				ioBufferConf: IOBufferConfig{
					Enabled: false,
				},
				mainFunc: Main{
					MainFuncName: "failed",
				},
			},
			expectedErrMsg:    "failed to call failed: function call failed",
			expectedTaskState: drivers.TaskStateUnknown,
		},
		{
			name: "empty result size",
			tHandle: &taskHandle{
				logger:       hclog.Default(),
				completionCh: make(chan struct{}),
				instance:     &mockInstance{},
				ioBufferConf: IOBufferConfig{
					Enabled:       true,
					Size:          IOBufValidSize,
					InputValue:    IOBufValidInput,
					IOBufFuncName: "workingIOBufFunc",
				},
				mainFunc: Main{
					MainFuncName: "unsuccessWASMExecution",
				},
			},
			expectedErrMsg:    "unsuccessful WASM call",
			expectedTaskState: drivers.TaskStateUnknown,
		},
		{
			name: "unable to open writer",
			tHandle: &taskHandle{
				logger:       hclog.Default(),
				completionCh: make(chan struct{}),
				instance:     &mockInstance{},
				taskConfig: &drivers.TaskConfig{
					StdoutPath: "nonExistingFile",
				},
				ioBufferConf: IOBufferConfig{
					Enabled:       true,
					Size:          IOBufValidSize,
					InputValue:    IOBufValidInput,
					IOBufFuncName: "workingIOBufFunc",
				},
				mainFunc: Main{
					MainFuncName: "workingMainFunc",
				},
			},
			expectedErrMsg:    "open nonExistingFile: no such file or directory",
			expectedTaskState: drivers.TaskStateUnknown,
		},
		{
			name: "successful run with buffer",
			tHandle: &taskHandle{
				logger:       hclog.Default(),
				completionCh: make(chan struct{}),
				instance:     &mockInstance{},
				taskConfig: &drivers.TaskConfig{
					StdoutPath: func() string {
						stdOutFile, err := os.CreateTemp("", "stdOutPath")
						if err != nil {
							t.Fatal("unable to create std out file for test")
						}
						stdOutFile.Close()

						return stdOutFile.Name()
					}(),
				},
				ioBufferConf: IOBufferConfig{
					Enabled:       true,
					Size:          IOBufValidSize,
					InputValue:    IOBufValidInput,
					IOBufFuncName: "workingIOBufFunc",
				},
				mainFunc: Main{
					MainFuncName: "workingMainFunc",
				},
			},
			expectedTaskState: drivers.TaskStateExited,
			expectedErrMsg:    "",
			stdOutPathPresent: true,
		},
		{
			name: "successful run without buffer",
			tHandle: &taskHandle{
				logger:       hclog.Default(),
				completionCh: make(chan struct{}),
				instance:     &mockInstance{},
				taskConfig: &drivers.TaskConfig{
					StdoutPath: func() string {
						stdOutFile, err := os.CreateTemp("", "stdOutPath")
						if err != nil {
							t.Fatal("unable to create std out file for test")
						}
						stdOutFile.Close()

						return stdOutFile.Name()
					}(),
				},
				ioBufferConf: IOBufferConfig{
					Enabled: false,
				},
				mainFunc: Main{
					MainFuncName: "workingMainFunc",
				},
			},
			expectedTaskState: drivers.TaskStateExited,
			expectedErrMsg:    "",
			stdOutPathPresent: true,
		},
	}

	for _, tCase := range tCases {
		t.Run(tCase.name, func(t *testing.T) {
			if tCase.stdOutPathPresent {
				defer os.Remove(tCase.tHandle.taskConfig.StdoutPath)
			}

			tCase.tHandle.run()

			if tCase.expectedErrMsg == "" {
				assert.Nil(t, tCase.tHandle.exitResult.Err)
			} else {
				assert.Equal(t, tCase.expectedErrMsg, tCase.tHandle.exitResult.Err.Error())
			}

			assert.Equal(t, tCase.expectedTaskState, tCase.tHandle.procState)
		})
	}
}
