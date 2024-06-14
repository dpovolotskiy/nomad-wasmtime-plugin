package wasmtime

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"github.com/bluele/gcache"
	"github.com/bytecodealliance/wasmtime-go"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/drivers/shared/eventer"
	"github.com/hashicorp/nomad/plugins/base"
	"github.com/hashicorp/nomad/plugins/device"
	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/hashicorp/nomad/plugins/shared/hclspec"
	"github.com/hashicorp/nomad/plugins/shared/structs"
)

const (
	// pluginName is the name of the plugin
	// this is used for logging and (along with the version) for uniquely
	// identifying plugin binaries fingerprinted by the client.
	pluginName = "wasmtime"

	// pluginVersion allows the client to identify and use newer versions of
	// an installed plugin.
	pluginVersion = "v0.1.0"

	// fingerprintPeriod is the interval at which the plugin will send
	// fingerprint responses.
	fingerprintPeriod = 30 * time.Second

	// taskHandleVersion is the version of task handle which this plugin sets
	// and understands how to decode
	// this is used to allow modification and migration of the task schema
	// used by the plugin.
	taskHandleVersion = 1
)

var (
	// pluginInfo describes the plugin.
	pluginInfo = &base.PluginInfoResponse{
		Type:              base.PluginTypeDriver,
		PluginApiVersions: []string{drivers.ApiVersion010},
		PluginVersion:     pluginVersion,
		Name:              pluginName,
	}

	cacheExpirationBlock = hclspec.NewObject(map[string]*hclspec.Spec{
		"enabled": hclspec.NewDefault(
			hclspec.NewAttr("enabled", "bool", false),
			hclspec.NewLiteral(`true`),
		),
		"entryTTL": hclspec.NewDefault(
			hclspec.NewAttr("entryTTL", "number", false),
			hclspec.NewLiteral(`600`),
		),
	})

	preCacheBlock = hclspec.NewObject(map[string]*hclspec.Spec{
		"enabled": hclspec.NewDefault(
			hclspec.NewAttr("enabled", "bool", false),
			hclspec.NewLiteral(`false`),
		),
		"modulesDir": hclspec.NewAttr("modulesDir", "string", false),
	})

	// configSpec is the specification of the plugin's configuration
	// this is used to validate the configuration specified for the plugin
	// on the client.
	// this is not global, but can be specified on a per-client basis.
	configSpec = hclspec.NewObject(map[string]*hclspec.Spec{
		// The schema should be defined using HCL specs and it will be used to
		// validate the agent configuration provided by the user in the
		// `plugin` stanza (https://www.nomadproject.io/docs/configuration/plugin.html).
		//
		// For example, for the schema below a valid configuration would be:
		//
		//   plugin "wasmtime-driver" {
		//     config {
		//       cache {
		//         enabled = true
		//         type = "lfu"
		//         size = 5
		//         expiration = {
		//           enabled = true
		//           entryTTL = 600
		//         }
		//         preCache {
		//           enabled = false
		//         }
		//       }
		//     }
		//   }
		"cache": hclspec.NewDefault(hclspec.NewBlock("cache", false, hclspec.NewObject(map[string]*hclspec.Spec{
			"enabled": hclspec.NewDefault(
				hclspec.NewAttr("enabled", "bool", false),
				hclspec.NewLiteral(`true`),
			),
			"type": hclspec.NewDefault(
				hclspec.NewAttr("type", "string", false),
				hclspec.NewLiteral(`"lfu"`),
			),
			"size": hclspec.NewDefault(
				hclspec.NewAttr("size", "number", false),
				hclspec.NewLiteral(`5`),
			),
			"expiration": hclspec.NewDefault(
				hclspec.NewBlock("expiration", false, cacheExpirationBlock),
				hclspec.NewLiteral(`{
					enabled = true
					entryTTL = 600
				}`),
			),
			"preCache": hclspec.NewDefault(
				hclspec.NewBlock("preCache", false, preCacheBlock),
				hclspec.NewLiteral(`{ enabled = false }`),
			),
		})), hclspec.NewLiteral(`{
				enabled = true
				type = "lfu"
				size = 5
				expiration = {
					enabled = true
					entryTTL = 600
				}
				preCache = {
					enabled = false
				}
		}`)),
	})

	// taskConfigSpec is the specification of the plugin's configuration for
	// a task
	// this is used to validated the configuration specified for the plugin
	// when a job is submitted.
	taskConfigSpec = hclspec.NewObject(map[string]*hclspec.Spec{
		// The schema should be defined using HCL specs and it will be used to
		// validate the task configuration provided by the user when they
		// submit a job.
		//
		// For example, for the schema below a valid task would be:
		//   job "example" {
		//     group "example" {
		//       task "say-hello" {
		//         driver = "wasmtime-driver-plugin"
		//         config {
		//           modulePath = "/absolute/path/to/wasm/module"
		//           ioBuffer {
		//             enabled = false
		//           }
		//           main {
		//             mainFuncName = "handle_buffer"
		//           }
		//         }
		//       }
		//     }
		//   }
		"modulePath": hclspec.NewAttr("modulePath", "string", true),
		"ioBuffer": hclspec.NewDefault(hclspec.NewBlock("ioBuffer", false, hclspec.NewObject(map[string]*hclspec.Spec{
			"enabled": hclspec.NewDefault(
				hclspec.NewAttr("enabled", "bool", false),
				hclspec.NewLiteral(`false`),
			),
			"size": hclspec.NewDefault(
				hclspec.NewAttr("size", "number", false),
				hclspec.NewLiteral(`4096`),
			),
			"inputValue": hclspec.NewAttr("inputValue", "string", false),
			"IOBufFuncName": hclspec.NewDefault(
				hclspec.NewAttr("IOBufFuncName", "string", false),
				hclspec.NewLiteral(`"alloc"`),
			),
			"args": hclspec.NewAttr("args", "list(number)", false),
		})), hclspec.NewLiteral(`{ enabled = false }`)),
		"main": hclspec.NewDefault(hclspec.NewBlock("main", false, hclspec.NewObject(map[string]*hclspec.Spec{
			"mainFuncName": hclspec.NewDefault(
				hclspec.NewAttr("mainFuncName", "string", false),
				hclspec.NewLiteral(`"handle_buffer"`),
			),
			"args": hclspec.NewAttr("args", "list(number)", false),
		})), hclspec.NewLiteral(`{ mainFuncName = "handle_buffer" }`)),
	})

	// capabilities indicates what optional features this driver supports
	// this should be set according to the target run time.
	capabilities = &drivers.Capabilities{}
)

type PreCacheConfig struct {
	Enabled bool `codec:"enabled"`
	// ModulesDir specify path to directory from where all modules will be pre-cached.
	ModulesDir string `codec:"modulesDir"`
}

type ExpirationConfig struct {
	Enabled bool `codec:"enabled"`
	// EntryTTL specify TTL of cache entry in seconds
	EntryTTL int `codec:"entryTTL"`
}

type CacheConfig struct {
	Enabled bool `codec:"enabled"`
	Size    int  `codec:"size"`
	// Cache type one of: lfu, lru, arc or simple.
	Type       string           `codec:"type"`
	PreCache   PreCacheConfig   `codec:"preCache"`
	Expiration ExpirationConfig `codec:"expiration"`
}

// Config contains configuration information for the plugin.
type Config struct {
	// This struct is the decoded version of the schema defined in the
	// configSpec variable above. It's used to convert the HCL configuration
	// passed by the Nomad agent into Go contructs.
	Cache CacheConfig `codec:"cache"`
}

// TaskConfig contains configuration information for a task that runs with
// this plugin.
type TaskConfig struct {
	// This struct is the decoded version of the schema defined in the
	// taskConfigSpec variable above. It's used to convert the string
	// configuration for the task into Go constructs.
	ModulePath string         `codec:"modulePath"`
	IOBuffer   IOBufferConfig `codec:"ioBuffer"`
	Main       Main           `codec:"main"`
}

type IOBufferConfig struct {
	Enabled bool `codec:"enabled"`
	// Size defines the length of the buffer created in the WASM module.
	Size int32 `codec:"size"`
	// InputValue defines the value passed to the WASM module buffer.
	InputValue string `codec:"inputValue"`
	// IOBufFuncName defines the name of the exported function in the WASM module
	// that returns the address of the start of the buffer created in the WASM module.
	IOBufFuncName string `codec:"IOBufFuncName"`
	// Args stores args that can be passed to the corresponding function.
	Args []int32 `codec:"args"`
}

type Main struct {
	// MainFuncName defines the function that will be called to handle the input.
	MainFuncName string `codec:"mainFuncName"`
	// Args stores args that can be passed to the corresponding function.
	Args []int32 `codec:"args"`
}

// TaskState is the runtime state which is encoded in the handle returned to
// Nomad client.
// This information is needed to rebuild the task state and handler during
// recovery.
type TaskState struct {
	ReattachConfig *structs.ReattachConfig
	TaskConfig     *drivers.TaskConfig
	StartedAt      time.Time
}

type WasmtimeDriverPlugin struct {
	// eventer is used to handle multiplexing of TaskEvents calls such that an
	// event can be broadcast to all callers
	eventer *eventer.Eventer

	// config is the plugin configuration set by the SetConfig RPC
	config *Config

	// nomadConfig is the client config from Nomad
	nomadConfig *base.ClientDriverConfig

	// tasks is the in memory datastore mapping taskIDs to driver handles
	tasks *taskStore

	// ctx is the context for the driver. It is passed to other subsystems to
	// coordinate shutdown
	ctx context.Context

	// signalShutdown is called when the driver is shutting down and cancels
	// the ctx passed to any subsystems
	signalShutdown context.CancelFunc

	// logger will log to the Nomad agent
	logger hclog.Logger

	// modulesCache is the cache that allow to store last recently used modules in memory
	modulesCache gcache.Cache
}

// NewPlugin returns a new example driver plugin.
func NewPlugin(logger hclog.Logger) drivers.DriverPlugin {
	ctx, cancel := context.WithCancel(context.Background())
	logger = logger.Named(pluginName)

	return &WasmtimeDriverPlugin{
		eventer:        eventer.NewEventer(ctx, logger),
		config:         &Config{},
		tasks:          newTaskStore(),
		ctx:            ctx,
		signalShutdown: cancel,
		logger:         logger,
	}
}

// PluginInfo returns information describing the plugin.
func (d *WasmtimeDriverPlugin) PluginInfo() (*base.PluginInfoResponse, error) {
	return pluginInfo, nil
}

// ConfigSchema returns the plugin configuration schema.
func (d *WasmtimeDriverPlugin) ConfigSchema() (*hclspec.Spec, error) {
	return configSpec, nil
}

// SetConfig is called by the client to pass the configuration for the plugin.
func (d *WasmtimeDriverPlugin) SetConfig(cfg *base.Config) error {
	var config Config
	if len(cfg.PluginConfig) != 0 {
		if err := base.MsgPackDecode(cfg.PluginConfig, &config); err != nil {
			return err
		}
	}

	// Save the configuration to the plugin
	d.config = &config

	// Validation of passed configuration
	cacheConf := d.config.Cache
	if cacheConf.Size <= 0 {
		return fmt.Errorf("cache size must be > 0, but specified %v", cacheConf.Size)
	}

	if cacheConf.Expiration.Enabled && cacheConf.Expiration.EntryTTL <= 0 {
		return fmt.Errorf("cache entry time-to-live must be > 0, but specified %v", cacheConf.Expiration.EntryTTL)
	}

	// Save the Nomad agent configuration
	if cfg.AgentConfig != nil {
		d.nomadConfig = cfg.AgentConfig.Driver
	}

	// Here you can use the config values to initialize any resources that are
	// shared by all tasks that use this driver, such as a daemon process.
	if cacheConf.Enabled {
		if err := d.configureCache(); err != nil {
			return err
		}
	}

	return nil
}

//nolint:gocyclo
func (d *WasmtimeDriverPlugin) configureCache() error {
	cacheConf := d.config.Cache

	cacheBuilder := gcache.New(cacheConf.Size)

	if cacheConf.Expiration.Enabled {
		cacheBuilder.Expiration(time.Second * time.Duration(cacheConf.Expiration.EntryTTL))
	}

	switch cacheConf.Type {
	case gcache.TYPE_LFU:
		cacheBuilder.LFU()
	case gcache.TYPE_ARC:
		cacheBuilder.ARC()
	case gcache.TYPE_LRU:
		cacheBuilder.LRU()
	case gcache.TYPE_SIMPLE:
		cacheBuilder.Simple()
	default:
		return fmt.Errorf("unexpected cache type specified, expected types: [lfu, arc, lru, simple], but specififed %s",
			cacheConf.Type)
	}

	d.modulesCache = cacheBuilder.Build()

	if cacheConf.PreCache.Enabled {
		var modulesPath []string

		err := filepath.Walk(cacheConf.PreCache.ModulesDir, func(path string, info fs.FileInfo, err error) error {
			if !info.IsDir() && strings.HasSuffix(info.Name(), ".wasm") {
				modulesPath = append(modulesPath, path)
			}
			return nil
		})

		if err != nil {
			return fmt.Errorf("unable to get WASM modules for pre-cache from %s directory: %v",
				cacheConf.PreCache.ModulesDir, err)
		}

		if len(modulesPath) > cacheConf.Size {
			return fmt.Errorf("cache size (%v) must not be less then number of pre-cached modules (%v)",
				cacheConf.Size, len(modulesPath))
		}

		loadEngineConfig := wasmtime.NewConfig()
		loadEngineConfig.SetEpochInterruption(true)
		loadEngine := wasmtime.NewEngineWithConfig(loadEngineConfig)

		for _, modulePath := range modulesPath {
			wasmModule, err := wasmtime.NewModuleFromFile(loadEngine, modulePath)
			if err != nil {
				return fmt.Errorf("unable to load WASM module (%v) from file: %v", modulePath, err)
			}

			serModule, err := wasmModule.Serialize()
			if err != nil {
				return fmt.Errorf("unable to serialize WASM module (%v): %v", modulePath, err)
			}

			if err := d.modulesCache.Set(modulePath, serModule); err != nil {
				return fmt.Errorf("unable to cache WASM module (%v)", modulePath)
			}

			d.logger.Trace("WASM module pre-cached", "module", modulePath)
		}

		if cacheConf.Expiration.Enabled {
			d.logger.Warn("since expiration enabled for cache all pre-cached modules also will be removed from cache after TTL",
				"TTL", hclog.Fmt("%d seconds", cacheConf.Expiration.EntryTTL))
		}
	}

	return nil
}

// TaskConfigSchema returns the HCL schema for the configuration of a task.
func (d *WasmtimeDriverPlugin) TaskConfigSchema() (*hclspec.Spec, error) {
	return taskConfigSpec, nil
}

// Capabilities returns the features supported by the driver.
func (d *WasmtimeDriverPlugin) Capabilities() (*drivers.Capabilities, error) {
	return capabilities, nil
}

// Fingerprint returns a channel that will be used to send health information
// and other driver specific node attributes.
func (d *WasmtimeDriverPlugin) Fingerprint(ctx context.Context) (<-chan *drivers.Fingerprint, error) {
	ch := make(chan *drivers.Fingerprint)
	go d.handleFingerprint(ctx, ch)

	return ch, nil
}

// handleFingerprint manages the channel and the flow of fingerprint data.
func (d *WasmtimeDriverPlugin) handleFingerprint(ctx context.Context, ch chan<- *drivers.Fingerprint) {
	defer close(ch)

	// Nomad expects the initial fingerprint to be sent immediately
	ticker := time.NewTimer(0)

	for {
		select {
		case <-ctx.Done():
			return
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			// after the initial fingerprint we can set the proper fingerprint
			// period
			ticker.Reset(fingerprintPeriod)
			ch <- d.buildFingerprint()
		}
	}
}

// buildFingerprint returns the driver's fingerprint data.
func (d *WasmtimeDriverPlugin) buildFingerprint() *drivers.Fingerprint {
	fp := &drivers.Fingerprint{
		Attributes:        map[string]*structs.Attribute{},
		Health:            drivers.HealthStateHealthy,
		HealthDescription: drivers.DriverHealthy,
	}

	// Fingerprinting is used by the plugin to relay two important information
	// to Nomad: health state and node attributes.
	//
	// If the plugin reports to be unhealthy, or doesn't send any fingerprint
	// data in the expected interval of time, Nomad will restart it.
	//
	// Node attributes can be used to report any relevant information about
	// the node in which the plugin is running (specific library availability,
	// installed versions of a software etc.). These attributes can then be
	// used by an operator to set job constrains.

	return fp
}

// StartTask returns a task handle and a driver network if necessary.
func (d *WasmtimeDriverPlugin) StartTask(cfg *drivers.TaskConfig) (*drivers.TaskHandle, *drivers.DriverNetwork, error) {
	if _, ok := d.tasks.Get(cfg.ID); ok {
		return nil, nil, fmt.Errorf("task with ID %q already started", cfg.ID)
	}

	var driverConfig TaskConfig
	if err := cfg.DecodeDriverConfig(&driverConfig); err != nil {
		return nil, nil, fmt.Errorf("failed to decode driver config: %v", err)
	}

	d.logger.Info("starting task", "driver_cfg", hclog.Fmt("%+v", driverConfig))

	handle := drivers.NewTaskHandle(taskHandleVersion)
	handle.Config = cfg

	engineConfig := wasmtime.NewConfig()
	engineConfig.SetEpochInterruption(true)

	engine := wasmtime.NewEngineWithConfig(engineConfig)

	store := wasmtime.NewStore(engine)
	store.SetEpochDeadline(1)

	// Once the task is started you will need to store any relevant runtime
	// information in a taskHandle and TaskState. The taskHandle will be
	// stored in-memory in the plugin and will be used to interact with the
	// task.
	//
	// The TaskState will be returned to the Nomad client inside a
	// drivers.TaskHandle instance. This TaskHandle will be sent back to plugin
	// if the task ever needs to be recovered, so the TaskState should contain
	// enough information to handle that.

	h := &taskHandle{
		taskConfig:       cfg,
		procState:        drivers.TaskStateRunning,
		startedAt:        time.Now().Round(time.Millisecond),
		logger:           d.logger,
		modulePath:       driverConfig.ModulePath,
		ioBufferConf:     driverConfig.IOBuffer,
		mainFunc:         driverConfig.Main,
		wasmModulesCache: d.modulesCache,
		store:            store,
		completionCh:     make(chan struct{}),
	}

	driverState := TaskState{
		ReattachConfig: &structs.ReattachConfig{},
		TaskConfig:     cfg,
		StartedAt:      h.startedAt,
	}

	if err := handle.SetDriverState(&driverState); err != nil {
		return nil, nil, fmt.Errorf("failed to set driver state: %v", err)
	}

	d.tasks.Set(cfg.ID, h)
	go h.run()

	return handle, nil, nil
}

// RecoverTask recreates the in-memory state of a task from a TaskHandle.
func (d *WasmtimeDriverPlugin) RecoverTask(_handle *drivers.TaskHandle) error {
	return nil
}

// WaitTask returns a channel used to notify Nomad when a task exits.
func (d *WasmtimeDriverPlugin) WaitTask(ctx context.Context, taskID string) (<-chan *drivers.ExitResult, error) {
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return nil, drivers.ErrTaskNotFound
	}

	ch := make(chan *drivers.ExitResult)
	go d.handleWait(ctx, handle, ch)

	return ch, nil
}

func (d *WasmtimeDriverPlugin) handleWait(ctx context.Context, handle *taskHandle, ch chan *drivers.ExitResult) {
	defer close(ch)

	// When a result is sent in the result channel Nomad will stop the task and
	// emit an event that an operator can use to get an insight on why the task
	// stopped.
	//

	for {
		select {
		case <-ctx.Done():
			return
		case <-d.ctx.Done():
			return
		case <-handle.completionCh:
			ch <- handle.exitResult
		}
	}
}

// StopTask stops a running task with the given signal and within the timeout window.
func (d *WasmtimeDriverPlugin) StopTask(taskID string, _timeout time.Duration, _signal string) error {
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return drivers.ErrTaskNotFound
	}

	handle.stop()

	return nil
}

// DestroyTask cleans up and removes a task that has terminated.
func (d *WasmtimeDriverPlugin) DestroyTask(taskID string, force bool) error {
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return drivers.ErrTaskNotFound
	}

	if handle.IsRunning() && !force {
		return errors.New("cannot destroy running task")
	}

	// Destroying a task includes removing any resources used by task and any
	// local references in the plugin. If force is set to true the task should
	// be destroyed even if it's currently running.
	//

	if handle.IsRunning() && force {
		handle.stop()
	}

	d.tasks.Delete(taskID)

	return nil
}

// InspectTask returns detailed status information for the referenced taskID.
func (d *WasmtimeDriverPlugin) InspectTask(taskID string) (*drivers.TaskStatus, error) {
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return nil, drivers.ErrTaskNotFound
	}

	return handle.TaskStatus(), nil
}

// TaskStats returns a channel which the driver should send stats to at the given interval.
func (d *WasmtimeDriverPlugin) TaskStats(ctx context.Context, taskID string, interval time.Duration) (<-chan *drivers.TaskResourceUsage, error) {
	_, ok := d.tasks.Get(taskID)
	if !ok {
		return nil, drivers.ErrTaskNotFound
	}

	// This function returns a channel that Nomad will use to listen for task
	// stats (e.g., CPU and memory usage) in a given interval. It should send
	// stats until the context is canceled or the task stops running.
	ch := make(chan *drivers.TaskResourceUsage)
	go d.handleTaskStats(ctx, interval, ch)

	return ch, nil
}

func (d *WasmtimeDriverPlugin) handleTaskStats(ctx context.Context, interval time.Duration, ch chan<- *drivers.TaskResourceUsage) {
	defer close(ch)

	ticker := time.NewTicker(interval)

	for {
		select {
		case <-ctx.Done():
			return
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			ch <- &drivers.TaskResourceUsage{
				ResourceUsage: &drivers.ResourceUsage{
					MemoryStats: &drivers.MemoryStats{},
					CpuStats:    &drivers.CpuStats{},
					DeviceStats: make([]*device.DeviceGroupStats, 0),
				},
			}
		}
	}
}

// TaskEvents returns a channel that the plugin can use to emit task related events.
func (d *WasmtimeDriverPlugin) TaskEvents(ctx context.Context) (<-chan *drivers.TaskEvent, error) {
	return d.eventer.TaskEvents(ctx)
}

// SignalTask forwards a signal to a task.
func (d *WasmtimeDriverPlugin) SignalTask(taskID string, _signal string) error {
	_, ok := d.tasks.Get(taskID)
	if !ok {
		return drivers.ErrTaskNotFound
	}

	return errors.New("this driver does not support signal forwarding")
}

// ExecTask returns the result of executing the given command inside a task.
func (d *WasmtimeDriverPlugin) ExecTask(_taskID string, _cmd []string, _timeout time.Duration) (*drivers.ExecTaskResult, error) {
	// TODO: implement driver specific logic to execute commands in a task.
	return nil, errors.New("this driver does not support exec")
}
