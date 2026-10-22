package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/HotLoop-io/hotloop-flow/internal/config"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/HotLoop-io/hotloop-flow/internal/plugin"
)

// PluginSetup is the plugin settings a flow test child is handed, because the
// child is this binary with no configuration file of its own. A flow that uses
// a plugin is tested with the same plugins, the same keys and the same grants
// the deploy would run it with.
type PluginSetup struct {
	Dir     string         `json:"dir"`
	Plugins config.Plugins `json:"plugins"`
}

func pluginSetup(cfg config.Config) PluginSetup {
	return PluginSetup{Dir: cfg.PluginsDir(), Plugins: cfg.Plugins}
}

// loadPlugins reads the plugin directory, registers what loaded into reg and
// says what it did. A refused plugin is logged with its reason and is never a
// startup failure: one bad plugin must not keep the plant's flows from coming
// up. A key or a grant the configuration can't parse is a startup failure,
// because the operator is looking at it and the alternative is guessing.
func loadPlugins(ctx context.Context, s PluginSetup, reg *node.Registry, log *slog.Logger) (*plugin.Host, error) {
	trust, err := plugin.LoadTrust(s.Plugins.TrustedKeys)
	if err != nil {
		return nil, fmt.Errorf("plugins: %w", err)
	}
	grants, err := plugin.ParseGrants(s.Plugins.Grants)
	if err != nil {
		return nil, err
	}
	host, err := plugin.Load(ctx, s.Dir, plugin.Policy{
		Trust: trust, Grants: grants,
		MaxMemoryMiB: s.Plugins.MaxMemoryMiB, MaxTimeout: s.Plugins.MaxTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("plugins: %w", err)
	}
	if err := host.Register(reg); err != nil {
		return nil, fmt.Errorf("plugins: %w", err)
	}
	for _, p := range host.Report().Plugins {
		log.Info("plugin loaded", "plugin", p.Name, "version", p.Version, "signedBy", p.SignedBy,
			"asks", strings.Join(p.Capabilities, ","), "granted", strings.Join(p.Granted, ","))
		if len(p.NotGranted) > 0 {
			log.Warn("a plugin asks for access it has not been granted; a flow using it will not deploy",
				"plugin", p.Name, "notGranted", strings.Join(p.NotGranted, ","))
		}
	}
	for _, r := range host.Refusals() {
		log.Error("plugin refused", "plugin", r.Name, "dir", r.Dir, "reason", r.Reason)
	}
	return host, nil
}
