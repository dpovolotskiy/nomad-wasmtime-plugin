package wasm

import (
	"testing"

	"github.com/bluele/gcache"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/plugins/base"
	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/hashicorp/nomad/plugins/shared/structs"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"

	"huawei.com/wasm-task-driver/wasm/engines"
	"huawei.com/wasm-task-driver/wasm/interfaces"
)

var (
	incorrectFormatConf = `-`

	defaultCacheSize = 5

	failedToPrePopulateErr       = errors.New("unable to pre populate")
	failedToInstantiateModuleErr = errors.New("unable to instantiate module")
)

type mockedEngine struct{}

func (e *mockedEngine) Name() string {
	return "mockedEngine"
}

func (e *mockedEngine) Init(_logger hclog.Logger, _moduleCache gcache.Cache) {}

func (e *mockedEngine) InstantiateModule(modulePath string) (interfaces.WasmInstance, error) {
	if modulePath == "failPath" {
		return nil, failedToInstantiateModuleErr
	}

	return &mockInstance{}, nil
}

func (e *mockedEngine) PrePopulateCache(modulesDir string) (int, error) {
	if modulesDir == "failedDir" {
		return 0, failedToPrePopulateErr
	}

	return defaultCacheSize, nil
}

func init() {
	hclog.Default().SetLevel(hclog.Debug)

	engines.Register(&mockedEngine{})
}

func TestInitializeEngine(t *testing.T) {
	tCases := []struct {
		name           string
		engineConf     EngineConfig
		expectedErrMsg string
	}{
		{
			name: "unable to find engine",
			engineConf: EngineConfig{
				Enabled: true,
				Name:    "incorrect",
			},
			expectedErrMsg: "unable to get engine incorrect: unable to find engine with name: incorrect: not found",
		},
		{
			name: "unable to build cache",
			engineConf: EngineConfig{
				Enabled: true,
				Name:    "mockedEngine",
				Cache: CacheConfig{
					Enabled: true,
					Size:    defaultCacheSize,
					Type:    "incorrect",
				},
			},
			expectedErrMsg: "unable to create cache for engine mockedEngine: unexpected cache type specified, expected types: [lfu, arc, lru, simple], but specified incorrect",
		},
		{
			name: "unable to pre populate",
			engineConf: EngineConfig{
				Enabled: true,
				Name:    "mockedEngine",
				Cache: CacheConfig{
					Enabled: true,
					Size:    defaultCacheSize,
					Type:    "lfu",
					PreCache: PreCacheConfig{
						Enabled:    true,
						ModulesDir: "failedDir",
					},
				},
			},
			expectedErrMsg: "unable to pre populate modules for engine mockedEngine from directory failedDir: unable to pre populate",
		},
		{
			name: "cache size less than number of modules for pre population",
			engineConf: EngineConfig{
				Enabled: true,
				Name:    "mockedEngine",
				Cache: CacheConfig{
					Enabled: true,
					Size:    defaultCacheSize - 1, // decrease cache size to initiate pre populate error
					Type:    "arc",
					PreCache: PreCacheConfig{
						Enabled:    true,
						ModulesDir: "fakeDir",
					},
				},
			},
			expectedErrMsg: "cache size (4) must not be less than number of pre-cached modules (5) for mockedEngine engine",
		},
		{
			name: "success engine initialization without cache",
			engineConf: EngineConfig{
				Enabled: true,
				Name:    "mockedEngine",
			},
		},
		{
			name: "success engine initialization with cache",
			engineConf: EngineConfig{
				Enabled: true,
				Name:    "mockedEngine",
				Cache: CacheConfig{
					Enabled: true,
					Size:    defaultCacheSize,
					Type:    "simple",
					PreCache: PreCacheConfig{
						Enabled:    true,
						ModulesDir: "fakeDir",
					},
					Expiration: ExpirationConfig{
						Enabled:  true,
						EntryTTL: 10,
					},
				},
			},
		},
	}

	for _, tCase := range tCases {
		t.Run(tCase.name, func(t *testing.T) {
			err := initializeEngine(hclog.Default(), tCase.engineConf)

			if tCase.expectedErrMsg == "" {
				assert.Nil(t, err)
			} else {
				assert.Equal(t, tCase.expectedErrMsg, err.Error())
			}
		})
	}
}

func TestBuildCache(t *testing.T) {
	tCases := []struct {
		name           string
		conf           CacheConfig
		expectedErrMsg string
	}{
		{
			name: "unexpected cache type",
			conf: CacheConfig{
				Enabled: true,
				Size:    defaultCacheSize,
				Expiration: ExpirationConfig{
					Enabled:  true,
					EntryTTL: 10,
				},
				Type: "incorrect",
			},
			expectedErrMsg: "unexpected cache type specified, expected types: [lfu, arc, lru, simple], but specified incorrect",
		},
		{
			name: "success cache build",
			conf: CacheConfig{
				Enabled: true,
				Size:    defaultCacheSize,
				Expiration: ExpirationConfig{
					Enabled:  true,
					EntryTTL: 10,
				},
				Type: "lru",
			},
		},
	}

	for _, tCase := range tCases {
		t.Run(tCase.name, func(t *testing.T) {
			cache, err := buildCache(tCase.conf)

			if tCase.expectedErrMsg == "" {
				assert.Nil(t, err)
				assert.NotNil(t, cache)
			} else {
				assert.Nil(t, cache)
				assert.Equal(t, tCase.expectedErrMsg, err.Error())
			}
		})
	}
}

func TestSetConfig(t *testing.T) {
	tCases := []struct {
		name           string
		config         []byte
		expectedErrMsg string
	}{
		{
			name:           "unable to decode config",
			config:         []byte(incorrectFormatConf),
			expectedErrMsg: "msgpack decode error [pos 1]: only encoded map or array can be decoded into a struct",
		},
		{
			name: "incorrect cache size",
			config: func() []byte {
				conf := Config{
					Engines: []EngineConfig{
						{
							Enabled: true,
							Name:    "mockedEngine",
							Cache: CacheConfig{
								Enabled: true,
								Size:    -1,
							},
						},
					},
				}

				var buf []byte

				_ = base.MsgPackEncode(&buf, conf)

				return buf
			}(),
			expectedErrMsg: "mockedEngine engine: cache size must be > 0, but specified -1",
		},
		{
			name: "incorrect TTL",
			config: func() []byte {
				conf := Config{
					Engines: []EngineConfig{
						{
							Enabled: true,
							Name:    "mockedEngine",
							Cache: CacheConfig{
								Enabled: true,
								Size:    defaultCacheSize,
								Expiration: ExpirationConfig{
									Enabled:  true,
									EntryTTL: -1,
								},
							},
						},
					},
				}

				var buf []byte

				_ = base.MsgPackEncode(&buf, conf)

				return buf
			}(),
			expectedErrMsg: "mockedEngine engine: cache entry time-to-live must be > 0, but specified -1",
		},
		{
			name: "engine not found",
			config: func() []byte {
				conf := Config{
					Engines: []EngineConfig{
						{
							Enabled: true,
							Name:    "absentEngine",
							Cache: CacheConfig{
								Enabled: false,
							},
						},
					},
				}

				var buf []byte

				_ = base.MsgPackEncode(&buf, conf)

				return buf
			}(),
			expectedErrMsg: "unable to get engine absentEngine: unable to find engine with name: absentEngine: not found",
		},
		{
			name: "success config set",
			config: func() []byte {
				conf := Config{
					Engines: []EngineConfig{
						{
							Enabled: true,
							Name:    "mockedEngine",
							Cache: CacheConfig{
								Enabled: true,
								Size:    defaultCacheSize,
								Type:    "lfu",
								Expiration: ExpirationConfig{
									Enabled:  true,
									EntryTTL: 10,
								},
								PreCache: PreCacheConfig{
									Enabled:    true,
									ModulesDir: "fakeDir",
								},
							},
						},
					},
				}

				var buf []byte

				_ = base.MsgPackEncode(&buf, conf)

				return buf
			}(),
		},
	}

	for _, tCase := range tCases {
		t.Run(tCase.name, func(t *testing.T) {
			d := &WasmTaskDriverPlugin{
				logger: hclog.Default(),
			}

			err := d.SetConfig(&base.Config{
				PluginConfig: tCase.config,
				AgentConfig: &base.AgentConfig{
					Driver: &base.ClientDriverConfig{},
				},
			})

			if tCase.expectedErrMsg == "" {
				assert.Nil(t, err)
			} else {
				assert.Equal(t, tCase.expectedErrMsg, err.Error())
			}
		})
	}
}

func TestBuildFingerprint(t *testing.T) {
	tCases := []struct {
		name                string
		driver              *WasmTaskDriverPlugin
		expectedFingerprint *drivers.Fingerprint
	}{
		{
			name: "single runtime driver",
			driver: &WasmTaskDriverPlugin{
				config: &Config{
					Engines: []EngineConfig{
						{
							Name: "wasmfake",
						},
					},
				},
			},
			expectedFingerprint: &drivers.Fingerprint{
				Health:            drivers.HealthStateHealthy,
				HealthDescription: drivers.DriverHealthy,
				Attributes: map[string]*structs.Attribute{
					"wasm.supported_runtimes": structs.NewStringAttribute("wasmfake"),
				},
			},
		},
		{
			name: "multiple runtimes driver",
			driver: &WasmTaskDriverPlugin{
				config: &Config{
					Engines: []EngineConfig{
						{
							Name: "wasmfake",
						},
						{
							Name: "wasmfake1",
						},
					},
				},
			},
			expectedFingerprint: &drivers.Fingerprint{
				Health:            drivers.HealthStateHealthy,
				HealthDescription: drivers.DriverHealthy,
				Attributes: map[string]*structs.Attribute{
					"wasm.supported_runtimes": structs.NewStringAttribute("wasmfake,wasmfake1"),
				},
			},
		},
	}

	for _, tCase := range tCases {
		t.Run(tCase.name, func(t *testing.T) {
			fingerprint := tCase.driver.buildFingerprint()

			assert.Equal(t, tCase.expectedFingerprint, fingerprint)
		})
	}
}

func TestStartTask(t *testing.T) {
	tCases := []struct {
		name           string
		driver         *WasmTaskDriverPlugin
		taskConfig     *drivers.TaskConfig
		expectedErrMsg string
	}{
		{
			name: "start already started task",
			driver: &WasmTaskDriverPlugin{
				tasks: func() *taskStore {
					ts := newTaskStore()
					ts.Set("0", nil)

					return ts
				}(),
			},
			taskConfig: &drivers.TaskConfig{
				ID: "0",
			},
			expectedErrMsg: "task with ID \"0\" already started",
		},
		{
			name: "unable to decode task config",
			driver: &WasmTaskDriverPlugin{
				tasks: newTaskStore(),
			},
			taskConfig: &drivers.TaskConfig{
				ID: "0",
			},
			expectedErrMsg: "failed to decode driver config: EOF",
		},
		{
			name: "unable to get engine",
			driver: &WasmTaskDriverPlugin{
				logger: hclog.Default(),
				tasks:  newTaskStore(),
			},
			taskConfig: func() *drivers.TaskConfig {
				pluginTask := TaskConfig{
					Engine: "absentEngine",
				}

				tk := &drivers.TaskConfig{
					ID: "0",
				}

				_ = tk.EncodeConcreteDriverConfig(pluginTask)

				return tk
			}(),
			expectedErrMsg: "failed to get absentEngine engine: unable to find engine with name: absentEngine: not found",
		},
		{
			name: "unable to instantiate module",
			driver: &WasmTaskDriverPlugin{
				logger: hclog.Default(),
				tasks:  newTaskStore(),
			},
			taskConfig: func() *drivers.TaskConfig {
				pluginTask := TaskConfig{
					Engine:     "mockedEngine",
					ModulePath: "failPath",
				}

				tk := &drivers.TaskConfig{
					ID: "0",
				}

				_ = tk.EncodeConcreteDriverConfig(pluginTask)

				return tk
			}(),
			expectedErrMsg: "failed to instantiate module failPath: unable to instantiate module",
		},
		{
			name: "success start task",
			driver: &WasmTaskDriverPlugin{
				logger: hclog.Default(),
				tasks:  newTaskStore(),
			},
			taskConfig: func() *drivers.TaskConfig {
				pluginTask := TaskConfig{
					Engine: "mockedEngine",
				}

				tk := &drivers.TaskConfig{
					ID: "0",
				}

				_ = tk.EncodeConcreteDriverConfig(pluginTask)

				return tk
			}(),
		},
	}

	for _, tCase := range tCases {
		t.Run(tCase.name, func(t *testing.T) {
			handle, _, err := tCase.driver.StartTask(tCase.taskConfig)

			if tCase.expectedErrMsg == "" {
				assert.Nil(t, err)
				assert.NotNil(t, handle)
			} else {
				assert.Nil(t, handle)
				assert.Equal(t, tCase.expectedErrMsg, err.Error())
			}
		})
	}
}

func TestDestroyTask(t *testing.T) {
	tCases := []struct {
		name           string
		driver         *WasmTaskDriverPlugin
		force          bool
		expectedErrMsg string
	}{
		{
			name: "task not found",
			driver: &WasmTaskDriverPlugin{
				tasks: newTaskStore(),
			},
			expectedErrMsg: "task not found for given id",
		},
		{
			name: "try to destroy running task",
			driver: &WasmTaskDriverPlugin{
				tasks: func() *taskStore {
					handle := &taskHandle{
						procState: drivers.TaskStateRunning,
					}

					ts := newTaskStore()
					ts.Set("0", handle)

					return ts
				}(),
			},
			expectedErrMsg: "cannot destroy running task",
		},
		{
			name: "destroy stopped task",
			driver: &WasmTaskDriverPlugin{
				tasks: func() *taskStore {
					handle := &taskHandle{
						procState: drivers.TaskStateExited,
					}

					ts := newTaskStore()
					ts.Set("0", handle)

					return ts
				}(),
			},
		},
		{
			name: "force destroy running task",
			driver: &WasmTaskDriverPlugin{
				tasks: func() *taskStore {
					handle := &taskHandle{
						procState: drivers.TaskStateRunning,
						instance:  &mockInstance{},
					}

					ts := newTaskStore()
					ts.Set("0", handle)

					return ts
				}(),
			},
			force: true,
		},
	}

	for _, tCase := range tCases {
		t.Run(tCase.name, func(t *testing.T) {
			err := tCase.driver.DestroyTask("0", tCase.force)

			if tCase.expectedErrMsg == "" {
				assert.Nil(t, err)
				assert.Equal(t, tCase.driver.tasks.size(), 0)
			} else {
				assert.Equal(t, tCase.expectedErrMsg, err.Error())
			}
		})
	}
}
