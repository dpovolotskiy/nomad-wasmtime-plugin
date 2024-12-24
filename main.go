package main

import (
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/plugins"

	"huawei.com/wasm-task-driver/wasm"

	_ "huawei.com/wasm-task-driver/wasm/engines/wasmedge"
	_ "huawei.com/wasm-task-driver/wasm/engines/wasmtime"
)

func main() {
	// Serve the plugin
	plugins.Serve(factory)
}

// factory returns a new instance of a nomad driver plugin.
func factory(log hclog.Logger) interface{} {
	return wasm.NewPlugin(log)
}
