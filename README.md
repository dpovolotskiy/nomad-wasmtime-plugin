WASM Task Driver
==========

This driver provides the ability to run WASM modules with
WASM runtimes on [Nomad](https://github.com/hashicorp/nomad).

Supported runtimes:
- [Wasmtime](https://github.com/bytecodealliance/wasmtime-go)
- [Wasmedge](https://github.com/second-state/WasmEdge-go)

## Example Job

The example below represents a simple job that passes two numbers to a WASM
module summing them up:

```hcl
job "sum" {
  datacenters = ["dc1"]
  type        = "batch"

  group "sum" {
    task "sum" {
      driver = "wasm-task-driver"

      config {
        engine = "wasmtime"
        modulePath = "example/wasm-modules/sum.wasm"
        main {
          mainFuncName = "sum"
          args = [123456, 678910]
        }
      }
    }
  }
}
```

```sh
nomad run sum.nomad

==> 2024-05-03T13:31:28Z: Monitoring evaluation "12300f89"
    2024-05-03T13:31:28Z: Evaluation triggered by job "sum"
    2024-05-03T13:31:29Z: Allocation "a4da2ca3" created: node "0e320013", group "sum"
    2024-05-03T13:31:29Z: Evaluation status changed: "pending" -> "complete"
==> 2024-05-03T13:31:29Z: Evaluation "12300f89" finished with status "complete"

nomad logs a4da2ca3

802366
```

## How To Build The Driver from source

To build this plugin from source you need to clone this repository somewhere.
Also make sure you use `go 1.21` or newer. After that you just need to execute
the following commands:

```sh
cd wasm-task-driver
make build
```

## Driver Configuration

* **engines** stanza is list of engine definitions:
  * **name** - Specifies the name of engine according to its extension
    name (like [here](wasm/engines/wasmtime/wasmtime_engine.go#L17) for `wasmtime` engine).
  * **enabled** - Defaults to `true`. Enables the defined wasm engine.
  * **cache** stanza:

    * **enabled** - Defaults to `true`. Allows serialized WASM modules to be cached
      to increase task execution speed.
    * **type** - Defaults to `lfu`. Allows to set one of the caching strategies.
      Allowed values: `lfu (least frequently used)`, `lru (least recently used)`,
      `arc (adaptive replacement cache)` and `simple` cache.
    * **size** - Default to `5`. Define the size of the cache, i.e. the maximum
      number of entries to be stored in the cache at the same time.
    * **expiration** stanza:

      * **enabled** - Defaults to `true`. Enables the expiration time for cached
        entries. After this time, entries are automatically removed from the
        cache.
      * **entryTTL** - Defaults to `600`. Specify the time after which the entry is
        removed from the cache.

    * **preCache** stanza:

      * **enabled** - Defaults to `false`. Enables pre-cache optimization that caches
        all WASM modules in specified directory and subdirectories. It allows not
        to spend additional time on loading and serialization of modules during
        execution.
      * **modulesDir** - Defaults to `""`. Specifies the path to the directory from which all modules
        (including subdirectories) are pre-cached.

## Task Configuration

* **engine** - Defines which WASM engine is used to execute the module.
* **modulePath** - Path to the WASM module to run.
* **ioBuffer** stanza:

  * **enabled** - Defaults to `false`. Enables the ability to pass some data
    (e.g. string) to the WASM module buffer. Buffer must be created on the
    WASM module side.
  * **size** - Defaults to `4096`. Defines the length of the buffer created
    in the WASM module.
  * **inputValue** - Defines the value passed to the WASM module buffer.
  * **IOBufFuncName** - Defaults to `get_io_buffer_ptr`. Defines the name of the
    exported function in the WASM module that returns the address of the start
    of the buffer created in the WASM module.
  * **args** - Stores arguments that can be passed to the corresponding function
    (specified in `IOBufFuncName` parameter).

* **main** stanza:

  * **mainFuncName** - Defaults to `handle_buffer`. Defines the name of the
    exported function in the WASM module to be called for execution.
  * **args** - Stores arguments that can be passed to the corresponding function
    (specified in `mainFuncName` parameter).

## How To Start Nomad With WASM Task Driver

### Local Development Setup

```sh
# Build the WASM Task Driver
make build

# Start Nomad with WASM Task Driver
nomad agent -dev -config=./example/config/agent.hcl -plugin-dir=$(pwd)/build/

# Run Nomad job
nomad run ./example/job/simple_job.nomad

# Check Nomad job status
nomad job status simple_example

# Check Nomad job output
nomad logs $(ALLOCATION_ID)
```

### Nomad Cluster Setup

1. Build Driver

   ```sh
   make build
   ```

2. Add the plugin configuration to the Nomad Client configuration file (only an
   example configuration is provided below, the required configuration must be
   adjusted according to the [documentation](#driver-configuration)):

   ```vim
   ...
   plugin "wasm-task-driver" {
     config {
       engines = [
         {
           name = "wasmtime"
           enabled = true
           cache {
             enabled = true
             type = "lru"
             size = 10
             expiration {
               enabled = true
               entryTTL = 10
             }
             preCache {
               enabled = true
               modulesDir = "/wasm-task-driver/example/wasm-modules"
             }
           }
         },
         {
           name = "wasmedge"
           enabled = true
         }
       ]
     }
   }
   ...
   ```

3. Start Nomad Client

   ```sh
   nomad agent -config=$(NOMAD_CLIENT_CONFIG_PATH) -plugin-dir=$(PLUGIN_DIR)
   ```

## Limitations

* Only `Int32` numbers can be passed to functions using the `args` option.
* The Wasmedge runtime doesn't support VM interruption.
