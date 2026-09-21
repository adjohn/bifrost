package mcptools

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/framework/configstore/tables"
)

func listPluginsTool() Tool {
	return Tool{
		name:        "list_plugins",
		description: "Configured plugins (name, enabled, custom, placement). Use describe_plugin for config.",
		schemaJSON: `{
  "type": "object",
  "properties": {
    "search": {"type": "string"}
  }
}`,
		noLogs: true,
		execute: func(ctx context.Context, deps *Deps, args map[string]any) (any, error) {
			gov, err := requireGovernance(deps)
			if err != nil {
				return nil, err
			}
			search, _, err := optionalStringArg(args, "search")
			if err != nil {
				return nil, err
			}
			rows, err := gov.GetPlugins(ctx)
			if err != nil {
				return nil, fmt.Errorf("list plugins failed: %w", err)
			}
			out := make([]map[string]any, 0, len(rows))
			for _, row := range rows {
				if row == nil {
					continue
				}
				if search != "" && !strings.Contains(strings.ToLower(row.Name), strings.ToLower(search)) {
					continue
				}
				item := projectPlugin(row)
				delete(item, "config")
				out = append(out, item)
			}
			if len(out) > MaxGovernanceRows {
				out = out[:MaxGovernanceRows]
			}
			return map[string]any{"plugins": out, "returned": len(out)}, nil
		},
	}
}

func describePluginTool() Tool {
	return Tool{
		name:        "describe_plugin",
		description: "One plugin: enabled, placement, order and stored config. Secrets in plugin config are whatever the plugin stored — treat them as sensitive.",
		schemaJSON: `{
  "type": "object",
  "properties": {
    "name": {"type": "string", "minLength": 1}
  },
  "required": ["name"]
}`,
		noLogs: true,
		execute: func(ctx context.Context, deps *Deps, args map[string]any) (any, error) {
			gov, err := requireGovernance(deps)
			if err != nil {
				return nil, err
			}
			name, err := stringArg(args, "name")
			if err != nil {
				return nil, err
			}
			row, err := gov.GetPlugin(ctx, name)
			if err != nil {
				if errors.Is(err, configstore.ErrNotFound) {
					return nil, fmt.Errorf("no plugin named %q", name)
				}
				return nil, fmt.Errorf("plugin lookup failed: %w", err)
			}
			return projectPlugin(row), nil
		},
	}
}

func createPluginTool() Tool {
	return Tool{
		name:        "create_plugin",
		description: "Register a plugin. Built-in plugins omit path. placement is pre_builtin or post_builtin.",
		schemaJSON: `{
  "type": "object",
  "properties": {
    "name": {"type": "string", "minLength": 1},
    "enabled": {"type": "boolean"},
    "config": {"type": "object"},
    "path": {"type": "string", "description": "Filesystem path for a custom plugin. Omit for built-ins."},
    "placement": {"type": "string", "enum": ["pre_builtin", "post_builtin"]},
    "order": {"type": "integer"}
  },
  "required": ["name"]
}`,
		noLogs:   true,
		mutating: true,
		execute: func(ctx context.Context, deps *Deps, args map[string]any) (any, error) {
			gov, err := requireGovernance(deps)
			if err != nil {
				return nil, err
			}
			name, err := stringArg(args, "name")
			if err != nil {
				return nil, err
			}
			enabled := true
			if flag, err := optionalBoolArg(args, "enabled"); err != nil {
				return nil, err
			} else if flag != nil {
				enabled = *flag
			}
			config, _, err := optionalObjectArg(args, "config")
			if err != nil {
				return nil, err
			}
			var path *string
			if p, ok, err := optionalStringArg(args, "path"); err != nil {
				return nil, err
			} else if ok {
				path = &p
			}
			placement, err := optionalPluginPlacement(args)
			if err != nil {
				return nil, err
			}
			order, err := optionalIntArg(args, "order")
			if err != nil {
				return nil, err
			}
			plugin := &tables.TablePlugin{
				Name:      name,
				Enabled:   enabled,
				Path:      path,
				Config:    config,
				Placement: placement,
				Order:     order,
			}
			if err := gov.CreatePlugin(ctx, plugin); err != nil {
				if errors.Is(err, configstore.ErrAlreadyExists) {
					return nil, fmt.Errorf("a plugin named %q already exists", name)
				}
				return nil, fmt.Errorf("create plugin failed: %w", err)
			}
			if deps.PluginRuntime != nil {
				if err := deps.PluginRuntime.ReloadPlugin(ctx, name, path, config, placement, order); err != nil {
					return projectPlugin(plugin), fmt.Errorf("stored but live reload failed: %w", err)
				}
			}
			return projectPlugin(plugin), nil
		},
	}
}

func updatePluginTool() Tool {
	return Tool{
		name:        "update_plugin",
		description: "Enable/disable a plugin or replace its config, path, placement or order.",
		schemaJSON: `{
  "type": "object",
  "properties": {
    "name": {"type": "string", "minLength": 1},
    "enabled": {"type": "boolean"},
    "config": {"type": "object"},
    "path": {"type": "string"},
    "placement": {"type": "string", "enum": ["pre_builtin", "post_builtin"]},
    "order": {"type": "integer"}
  },
  "required": ["name"]
}`,
		noLogs:   true,
		mutating: true,
		execute: func(ctx context.Context, deps *Deps, args map[string]any) (any, error) {
			gov, err := requireGovernance(deps)
			if err != nil {
				return nil, err
			}
			name, err := stringArg(args, "name")
			if err != nil {
				return nil, err
			}
			plugin, err := gov.GetPlugin(ctx, name)
			if err != nil {
				if errors.Is(err, configstore.ErrNotFound) {
					return nil, fmt.Errorf("no plugin named %q", name)
				}
				return nil, fmt.Errorf("plugin lookup failed: %w", err)
			}
			if flag, err := optionalBoolArg(args, "enabled"); err != nil {
				return nil, err
			} else if flag != nil {
				plugin.Enabled = *flag
			}
			if config, ok, err := optionalObjectArg(args, "config"); err != nil {
				return nil, err
			} else if ok {
				plugin.Config = config
			}
			if p, ok, err := optionalStringArg(args, "path"); err != nil {
				return nil, err
			} else if ok {
				plugin.Path = &p
			}
			if placement, err := optionalPluginPlacement(args); err != nil {
				return nil, err
			} else if placement != nil {
				plugin.Placement = placement
			}
			if order, err := optionalIntArg(args, "order"); err != nil {
				return nil, err
			} else if order != nil {
				plugin.Order = order
			}
			if err := gov.UpdatePlugin(ctx, plugin); err != nil {
				return nil, fmt.Errorf("update plugin failed: %w", err)
			}
			if deps.PluginRuntime != nil {
				if err := deps.PluginRuntime.ReloadPlugin(ctx, plugin.Name, plugin.Path, plugin.Config, plugin.Placement, plugin.Order); err != nil {
					return projectPlugin(plugin), fmt.Errorf("stored but live reload failed: %w", err)
				}
			}
			return projectPlugin(plugin), nil
		},
	}
}

func projectPlugin(plugin *tables.TablePlugin) map[string]any {
	out := map[string]any{
		"name":      plugin.Name,
		"enabled":   plugin.Enabled,
		"is_custom": plugin.IsCustom,
	}
	if plugin.Path != nil {
		out["path"] = *plugin.Path
	}
	if plugin.Placement != nil {
		out["placement"] = string(*plugin.Placement)
	}
	if plugin.Order != nil {
		out["order"] = *plugin.Order
	}
	if plugin.Config != nil {
		out["config"] = plugin.Config
	}
	return out
}

func optionalPluginPlacement(args map[string]any) (*schemas.PluginPlacement, error) {
	text, ok, err := optionalStringArg(args, "placement")
	if err != nil || !ok {
		return nil, err
	}
	placement := schemas.PluginPlacement(text)
	if placement != schemas.PluginPlacementPreBuiltin && placement != schemas.PluginPlacementPostBuiltin {
		return nil, fmt.Errorf("placement must be pre_builtin or post_builtin")
	}
	return &placement, nil
}
