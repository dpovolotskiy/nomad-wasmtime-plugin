package wasm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bluele/gcache"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/drivers/shared/eventer"
	"github.com/hashicorp/nomad/plugins/base"
	"github.com/hashicorp/nomad/plugins/device"
	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/hashicorp/nomad/plugins/shared/hclspec"
	"github.com/hashicorp/nomad/plugins/shared/structs"
	"huawei.com/wasm-task-driver/wasm/engines"
)

const (
	// fingerprintPrefix used to build attributes for plugin fingerprint.
	fingerprintPrefix = "wasm"

	// pluginName is the name of the plugin
	// this is used for logging and (along with the version) for uniquely
	// identifying plugin binaries fingerprinted by the client.
	pluginName = "wasm-task-driver"

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
		//   plugin "wasm-task-driver" {
		//     config {
		//       engines = [
		//         {
		//           name = "wasmtime"
		//           enabled = true
		//           cache {
		//             enabled = true
		//             type = "lru"
		//             size = 5
		//             expiration {
		//               enabled = true
		//               entryTTL = 600
		//             }
		//             preCache {
		//               enabled = false
		//             }
		//           }
		//         },
		//         {
		//            name = "wasmedge"
		//            enabled = true
		//         }
		//       ]
		//     }
		//   }
		"engines": hclspec.NewBlockList("engines", hclspec.NewObject(map[string]*hclspec.Spec{
			"name": hclspec.NewAttr("name", "string", true),
			"enabled": hclspec.NewDefault(
				hclspec.NewAttr("enabled", "bool", false),
				hclspec.NewLiteral(`true`),
			),
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
				"expiration": hclspec.NewDefault(hclspec.NewBlock("expiration", false, hclspec.NewObject(map[string]*hclspec.Spec{
					"enabled": hclspec.NewDefault(
						hclspec.NewAttr("enabled", "bool", false),
						hclspec.NewLiteral(`true`),
					),
					"entryTTL": hclspec.NewDefault(
						hclspec.NewAttr("entryTTL", "number", false),
						hclspec.NewLiteral(`600`),
					),
				})),
					hclspec.NewLiteral(`{
					enabled = true
					entryTTL = 600
				}`),
				),
				"preCache": hclspec.NewDefault(hclspec.NewBlock("preCache", false, hclspec.NewObject(map[string]*hclspec.Spec{
					"enabled": hclspec.NewDefault(
						hclspec.NewAttr("enabled", "bool", false),
						hclspec.NewLiteral(`false`),
					),
					"modulesDir": hclspec.NewDefault(
						hclspec.NewAttr("modulesDir", "string", false),
						hclspec.NewLiteral(`""`),
					),
				})),
					hclspec.NewLiteral(`{
							enabled = false
							modulesDir = ""
					}`),
				),
			})),
				hclspec.NewLiteral(`{
						enabled = true
						type = "lfu"
						size = 5
						expiration = {
							enabled = true
							entryTTL = 600
						}
						preCache = {
							enabled = false
							modulesDir = ""
						}
				}`),
			),
		})),
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
		//         driver = "wasm-task-driver"
		//         config {
		//           engine = "wasmtime"
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
		"engine":     hclspec.NewAttr("engine", "string", true),
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
		})),
			hclspec.NewLiteral(`{ enabled = false }`),
		),
		"main": hclspec.NewDefault(hclspec.NewBlock("main", false, hclspec.NewObject(map[string]*hclspec.Spec{
			"mainFuncName": hclspec.NewDefault(
				hclspec.NewAttr("mainFuncName", "string", false),
				hclspec.NewLiteral(`"handle_buffer"`),
			),
			"args": hclspec.NewAttr("args", "list(number)", false),
		})),
			hclspec.NewLiteral(`{ mainFuncName = "handle_buffer" }`),
		),
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

type EngineConfig struct {
	Name    string      `codec:"name"`
	Enabled bool        `codec:"enabled"`
	Cache   CacheConfig `codec:"cache"`
}

// Config contains configuration information for the plugin.
type Config struct {
	// This struct is the decoded version of the schema defined in the
	// configSpec variable above. It's used to convert the HCL configuration
	// passed by the Nomad agent into Go contructs.
	Engines []EngineConfig `codec:"engines"`
}

// TaskConfig contains configuration information for a task that runs with
// this plugin.
type TaskConfig struct {
	// This struct is the decoded version of the schema defined in the
	// taskConfigSpec variable above. It's used to convert the string
	// configuration for the task into Go constructs.
	Engine     string         `codec:"engine"`
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

type WasmTaskDriverPlugin struct {
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
}

// NewPlugin returns a new example driver plugin.
func NewPlugin(logger hclog.Logger) drivers.DriverPlugin {
	ctx, cancel := context.WithCancel(context.Background())
	logger = logger.Named(pluginName)

	return &WasmTaskDriverPlugin{
		eventer:        eventer.NewEventer(ctx, logger),
		config:         &Config{},
		tasks:          newTaskStore(),
		ctx:            ctx,
		signalShutdown: cancel,
		logger:         logger,
	}
}

// PluginInfo returns information describing the plugin.
func (d *WasmTaskDriverPlugin) PluginInfo() (*base.PluginInfoResponse, error) {
	return pluginInfo, nil
}

// ConfigSchema returns the plugin configuration schema.
func (d *WasmTaskDriverPlugin) ConfigSchema() (*hclspec.Spec, error) {
	return configSpec, nil
}

// SetConfig is called by the client to pass the configuration for the plugin.
func (d *WasmTaskDriverPlugin) SetConfig(cfg *base.Config) error {
	var config Config

	if len(cfg.PluginConfig) != 0 {
		if err := base.MsgPackDecode(cfg.PluginConfig, &config); err != nil {
			return err
		}
	}

	// Save the configuration to the plugin
	d.config = &config

	// Validation of passed configuration
	for _, engineConf := range d.config.Engines {
		cacheConf := engineConf.Cache

		if cacheConf.Enabled && cacheConf.Size <= 0 {
			return fmt.Errorf("%s engine: cache size must be > 0, but specified %v", engineConf.Name, cacheConf.Size)
		}

		if cacheConf.Expiration.Enabled && cacheConf.Expiration.EntryTTL <= 0 {
			return fmt.Errorf("%s engine: cache entry time-to-live must be > 0, but specified %v", engineConf.Name, cacheConf.Expiration.EntryTTL)
		}
	}

	// Save the Nomad agent configuration
	if cfg.AgentConfig != nil {
		d.nomadConfig = cfg.AgentConfig.Driver
	}

	// Here you can use the config values to initialize any resources that are
	// shared by all tasks that use this driver, such as a daemon process.
	for _, engineConf := range d.config.Engines {
		if err := initializeEngine(d.logger, engineConf); err != nil {
			return err
		}
	}

	return nil
}

func initializeEngine(logger hclog.Logger, engineConf EngineConfig) error {
	engine, err := engines.Get(engineConf.Name)
	if err != nil {
		return fmt.Errorf("unable to get engine %s: %v", engineConf.Name, err)
	}

	if engineConf.Cache.Enabled {
		newCache, err := buildCache(engineConf.Cache)
		if err != nil {
			return fmt.Errorf("unable to create cache for engine %s: %v", engineConf.Name, err)
		}

		engine.Init(logger, newCache)

		if engineConf.Cache.PreCache.Enabled {
			preCachedModulesNum, err := engine.PrePopulateCache(engineConf.Cache.PreCache.ModulesDir)
			if err != nil {
				return fmt.Errorf("unable to pre populate modules for engine %s from directory %s: %v", engineConf.Name, engineConf.Cache.PreCache.ModulesDir, err)
			}

			if preCachedModulesNum > engineConf.Cache.Size {
				return fmt.Errorf("cache size (%v) must not be less than number of pre-cached modules (%v) for %s engine",
					engineConf.Cache.Size, preCachedModulesNum, engineConf.Name)
			}

			if engineConf.Cache.Expiration.Enabled {
				logger.Warn("since expiration enabled for cache all pre-cached modules also will be removed from cache after TTL",
					"TTL", hclog.Fmt("%d seconds", engineConf.Cache.Expiration.EntryTTL), "engine", engineConf.Name)
			}
		}
	} else {
		engine.Init(logger, nil)
	}

	return nil
}

func buildCache(cacheConf CacheConfig) (gcache.Cache, error) {
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
		return nil, fmt.Errorf("unexpected cache type specified, expected types: [lfu, arc, lru, simple], but specified %s",
			cacheConf.Type)
	}

	return cacheBuilder.Build(), nil
}

// TaskConfigSchema returns the HCL schema for the configuration of a task.
func (d *WasmTaskDriverPlugin) TaskConfigSchema() (*hclspec.Spec, error) {
	return taskConfigSpec, nil
}

// Capabilities returns the features supported by the driver.
func (d *WasmTaskDriverPlugin) Capabilities() (*drivers.Capabilities, error) {
	return capabilities, nil
}

// Fingerprint returns a channel that will be used to send health information
// and other driver specific node attributes.
func (d *WasmTaskDriverPlugin) Fingerprint(ctx context.Context) (<-chan *drivers.Fingerprint, error) {
	ch := make(chan *drivers.Fingerprint)
	go d.handleFingerprint(ctx, ch)

	return ch, nil
}

// handleFingerprint manages the channel and the flow of fingerprint data.
func (d *WasmTaskDriverPlugin) handleFingerprint(ctx context.Context, ch chan<- *drivers.Fingerprint) {
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
func (d *WasmTaskDriverPlugin) buildFingerprint() *drivers.Fingerprint {
	fp := &drivers.Fingerprint{
		Attributes:        make(map[string]*structs.Attribute),
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

	supportedEngineNames := make([]string, 0, len(d.config.Engines))

	for _, engine := range d.config.Engines {
		supportedEngineNames = append(supportedEngineNames, engine.Name)
	}

	fp.Attributes[fmt.Sprintf("%s.%s", fingerprintPrefix, "supported_runtimes")] = structs.NewStringAttribute(
		strings.Join(supportedEngineNames, ","))

	return fp
}

// StartTask returns a task handle and a driver network if necessary.
func (d *WasmTaskDriverPlugin) StartTask(cfg *drivers.TaskConfig) (*drivers.TaskHandle, *drivers.DriverNetwork, error) {
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

	engine, err := engines.Get(driverConfig.Engine)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get %s engine: %v", driverConfig.Engine, err)
	}

	newInstance, err := engine.InstantiateModule(driverConfig.ModulePath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to instantiate module %s: %v", driverConfig.ModulePath, err)
	}

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
		taskConfig:   cfg,
		procState:    drivers.TaskStateRunning,
		startedAt:    time.Now().Round(time.Millisecond),
		logger:       d.logger,
		ioBufferConf: driverConfig.IOBuffer,
		mainFunc:     driverConfig.Main,
		instance:     newInstance,
		completionCh: make(chan struct{}),
	}

	driverState := TaskState{
		ReattachConfig: &structs.ReattachConfig{},
		TaskConfig:     cfg,
		StartedAt:      h.startedAt,
	}

	if err := handle.SetDriverState(&driverState); err != nil {
		// need to cleanup resources.
		h.instance.Cleanup()

		return nil, nil, fmt.Errorf("failed to set driver state: %v", err)
	}

	d.tasks.Set(cfg.ID, h)
	go h.run()

	return handle, nil, nil
}

// RecoverTask recreates the in-memory state of a task from a TaskHandle.
func (d *WasmTaskDriverPlugin) RecoverTask(_handle *drivers.TaskHandle) error {
	return nil
}

// WaitTask returns a channel used to notify Nomad when a task exits.
func (d *WasmTaskDriverPlugin) WaitTask(ctx context.Context, taskID string) (<-chan *drivers.ExitResult, error) {
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return nil, drivers.ErrTaskNotFound
	}

	ch := make(chan *drivers.ExitResult)
	go d.handleWait(ctx, handle, ch)

	return ch, nil
}

func (d *WasmTaskDriverPlugin) handleWait(ctx context.Context, handle *taskHandle, ch chan *drivers.ExitResult) {
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
func (d *WasmTaskDriverPlugin) StopTask(taskID string, _timeout time.Duration, _signal string) error {
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return drivers.ErrTaskNotFound
	}

	handle.instance.Stop()

	return nil
}

// DestroyTask cleans up and removes a task that has terminated.
func (d *WasmTaskDriverPlugin) DestroyTask(taskID string, force bool) error {
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
		handle.instance.Stop()
	}

	d.tasks.Delete(taskID)

	return nil
}

// InspectTask returns detailed status information for the referenced taskID.
func (d *WasmTaskDriverPlugin) InspectTask(taskID string) (*drivers.TaskStatus, error) {
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return nil, drivers.ErrTaskNotFound
	}

	return handle.TaskStatus(), nil
}

// TaskStats returns a channel which the driver should send stats to at the given interval.
func (d *WasmTaskDriverPlugin) TaskStats(ctx context.Context, taskID string, interval time.Duration) (<-chan *drivers.TaskResourceUsage, error) {
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

func (d *WasmTaskDriverPlugin) handleTaskStats(ctx context.Context, interval time.Duration, ch chan<- *drivers.TaskResourceUsage) {
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
func (d *WasmTaskDriverPlugin) TaskEvents(ctx context.Context) (<-chan *drivers.TaskEvent, error) {
	return d.eventer.TaskEvents(ctx)
}

// SignalTask forwards a signal to a task.
func (d *WasmTaskDriverPlugin) SignalTask(taskID string, _signal string) error {
	_, ok := d.tasks.Get(taskID)
	if !ok {
		return drivers.ErrTaskNotFound
	}

	return errors.New("this driver does not support signal forwarding")
}

// ExecTask returns the result of executing the given command inside a task.
func (d *WasmTaskDriverPlugin) ExecTask(_taskID string, _cmd []string, _timeout time.Duration) (*drivers.ExecTaskResult, error) {
	// TODO: implement driver specific logic to execute commands in a task.
	return nil, errors.New("this driver does not support exec")
}
