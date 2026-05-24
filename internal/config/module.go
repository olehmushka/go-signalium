package config

import (
	"os"

	werror "github.com/palantir/witchcraft-go-error"
	"go.uber.org/fx"
	"gopkg.in/yaml.v3"
)

// Module wires the typed install + runtime configs as fx-provided values. The
// witchcraft server later re-parses these same files for its own refreshable
// runtime updates; fx-side parsing here is the boot-time snapshot that every
// constructor (signal.TCPClient, storage.ObjectStore, worker.Worker, …) reads.
//
// Paths come in via the named string parameters main.go fx.Supply()s
// (`installConfigPath`, `runtimeConfigPath`).
var Module = fx.Module(
	"config",
	fx.Provide(
		LoadInstall,
		LoadRuntime,
		// Convenience providers for constructors that depend on a single
		// sub-block rather than the whole Install (storage.newMinioClient).
		func(i Install) MinIOConfig { return i.MinIO },
	),
)

// Paths captures the supplied install + runtime config file paths.
type Paths struct {
	fx.In

	Install string `name:"installConfigPath"`
	Runtime string `name:"runtimeConfigPath"`
}

// LoadInstall reads + parses var/conf/install.yml into Install. fx provider.
func LoadInstall(p Paths) (Install, error) {
	body, err := os.ReadFile(p.Install)
	if err != nil {
		return Install{}, werror.Wrap(err, "read install config",
			werror.SafeParam("path", p.Install))
	}
	var i Install
	if err := yaml.Unmarshal(body, &i); err != nil {
		return Install{}, werror.Wrap(err, "parse install config",
			werror.SafeParam("path", p.Install))
	}
	return i, nil
}

// LoadRuntime reads + parses var/conf/runtime.yml into Runtime. fx provider.
func LoadRuntime(p Paths) (Runtime, error) {
	body, err := os.ReadFile(p.Runtime)
	if err != nil {
		return Runtime{}, werror.Wrap(err, "read runtime config",
			werror.SafeParam("path", p.Runtime))
	}
	var r Runtime
	if err := yaml.Unmarshal(body, &r); err != nil {
		return Runtime{}, werror.Wrap(err, "parse runtime config",
			werror.SafeParam("path", p.Runtime))
	}
	return r, nil
}
