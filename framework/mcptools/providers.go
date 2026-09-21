package mcptools

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
)

func listProvidersTool() Tool {
	return Tool{
		name:        "list_providers",
		description: "Configured providers and how many keys each has. Never returns key material.",
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
			rows, err := gov.GetProviders(ctx)
			if err != nil {
				return nil, fmt.Errorf("list providers failed: %w", err)
			}
			out := make([]map[string]any, 0, len(rows))
			for i := range rows {
				name := rows[i].Name
				if search != "" && !strings.Contains(strings.ToLower(name), strings.ToLower(search)) {
					continue
				}
				item := map[string]any{
					"name":      name,
					"key_count": len(rows[i].Keys),
					"status":    rows[i].Status,
				}
				if rows[i].Description != "" {
					item["description"] = rows[i].Description
				}
				out = append(out, item)
			}
			return map[string]any{"providers": out, "returned": len(out)}, nil
		},
	}
}

func addProviderTool() Tool {
	return Tool{
		name:        "add_provider",
		description: "Register a provider (openai, anthropic, ...). Add keys afterwards with create_provider_key. Does not delete an existing provider.",
		schemaJSON: `{
  "type": "object",
  "properties": {
    "provider": {"type": "string", "minLength": 1, "description": "Provider name, e.g. openai."}
  },
  "required": ["provider"]
}`,
		noLogs:   true,
		mutating: true,
		execute: func(ctx context.Context, deps *Deps, args map[string]any) (any, error) {
			name, err := stringArg(args, "provider")
			if err != nil {
				return nil, err
			}
			provider := schemas.ModelProvider(name)
			config := configstore.ProviderConfig{}
			if deps.ProviderRuntime != nil {
				if err := deps.ProviderRuntime.AddProvider(ctx, provider, config); err != nil {
					return nil, fmt.Errorf("add provider failed: %w", err)
				}
				return map[string]any{"provider": name, "live": true}, nil
			}
			gov, err := requireGovernance(deps)
			if err != nil {
				return nil, err
			}
			if err := gov.AddProvider(ctx, provider, config); err != nil {
				if errors.Is(err, configstore.ErrAlreadyExists) {
					return nil, fmt.Errorf("provider %q already exists", name)
				}
				return nil, fmt.Errorf("add provider failed: %w", err)
			}
			return map[string]any{
				"provider": name,
				"live":     false,
				"note":     "stored; may not be live until the process reloads providers",
			}, nil
		},
	}
}

func listProviderKeysTool() Tool {
	return Tool{
		name:        "list_provider_keys",
		description: "Keys configured for one provider. Returns id, name, models and enabled — never the secret value.",
		schemaJSON: `{
  "type": "object",
  "properties": {
    "provider": {"type": "string", "minLength": 1}
  },
  "required": ["provider"]
}`,
		noLogs: true,
		execute: func(ctx context.Context, deps *Deps, args map[string]any) (any, error) {
			gov, err := requireGovernance(deps)
			if err != nil {
				return nil, err
			}
			name, err := stringArg(args, "provider")
			if err != nil {
				return nil, err
			}
			keys, err := gov.GetProviderKeys(ctx, schemas.ModelProvider(name))
			if err != nil {
				if errors.Is(err, configstore.ErrNotFound) {
					return nil, fmt.Errorf("no provider named %q", name)
				}
				return nil, fmt.Errorf("list provider keys failed: %w", err)
			}
			out := make([]map[string]any, 0, len(keys))
			for i := range keys {
				out = append(out, projectProviderKey(&keys[i]))
			}
			return map[string]any{"provider": name, "keys": out, "returned": len(out)}, nil
		},
	}
}

func createProviderKeyTool() Tool {
	return Tool{
		name:        "create_provider_key",
		description: "Add an API key to a provider. The secret goes in and is never returned. Names must be unique across providers.",
		schemaJSON: `{
  "type": "object",
  "properties": {
    "provider": {"type": "string", "minLength": 1},
    "name": {"type": "string", "minLength": 1},
    "value": {"type": "string", "minLength": 1, "description": "The provider API key. Stored, never returned."},
    "weight": {"type": "number", "description": "Load-balancing weight. Defaults to 1."}
  },
  "required": ["provider", "name", "value"]
}`,
		noLogs:   true,
		mutating: true,
		execute: func(ctx context.Context, deps *Deps, args map[string]any) (any, error) {
			providerName, err := stringArg(args, "provider")
			if err != nil {
				return nil, err
			}
			name, err := stringArg(args, "name")
			if err != nil {
				return nil, err
			}
			value, err := stringArg(args, "value")
			if err != nil {
				return nil, err
			}
			weight := 1.0
			if _, present := args["weight"]; present {
				weight, err = floatArg(args, "weight")
				if err != nil {
					return nil, err
				}
			}
			enabled := true
			key := schemas.Key{
				ID:      uuid.NewString(),
				Name:    name,
				Value:   *schemas.NewSecretVar(value),
				Weight:  weight,
				Enabled: &enabled,
			}
			provider := schemas.ModelProvider(providerName)
			if deps.ProviderRuntime != nil {
				if err := deps.ProviderRuntime.AddProviderKey(ctx, provider, key); err != nil {
					return nil, fmt.Errorf("create provider key failed: %w", err)
				}
			} else {
				gov, err := requireGovernance(deps)
				if err != nil {
					return nil, err
				}
				if err := gov.CreateProviderKey(ctx, provider, key); err != nil {
					if errors.Is(err, configstore.ErrAlreadyExists) {
						return nil, fmt.Errorf("a provider key named %q already exists", name)
					}
					return nil, fmt.Errorf("create provider key failed: %w", err)
				}
			}
			return map[string]any{
				"provider": providerName,
				"key":      projectProviderKey(&key),
				"note":     "the secret was stored and will never be returned",
			}, nil
		},
	}
}

func updateProviderKeyTool() Tool {
	return Tool{
		name:        "update_provider_key",
		description: "Rename a provider key, change its weight, or replace its secret. The new secret is never returned.",
		schemaJSON: `{
  "type": "object",
  "properties": {
    "provider": {"type": "string", "minLength": 1},
    "key_id": {"type": "string", "minLength": 1},
    "name": {"type": "string"},
    "value": {"type": "string", "description": "Replacement secret. Stored, never returned."},
    "weight": {"type": "number"},
    "enabled": {"type": "boolean"}
  },
  "required": ["provider", "key_id"]
}`,
		noLogs:   true,
		mutating: true,
		execute: func(ctx context.Context, deps *Deps, args map[string]any) (any, error) {
			gov, err := requireGovernance(deps)
			if err != nil && deps.ProviderRuntime == nil {
				return nil, err
			}
			providerName, err := stringArg(args, "provider")
			if err != nil {
				return nil, err
			}
			keyID, err := stringArg(args, "key_id")
			if err != nil {
				return nil, err
			}
			provider := schemas.ModelProvider(providerName)
			var existing schemas.Key
			if gov != nil {
				keys, err := gov.GetProviderKeys(ctx, provider)
				if err != nil {
					return nil, fmt.Errorf("list provider keys failed: %w", err)
				}
				found := false
				for i := range keys {
					if keys[i].ID == keyID {
						existing = keys[i]
						found = true
						break
					}
				}
				if !found {
					return nil, fmt.Errorf("no key %q on provider %q", keyID, providerName)
				}
			} else {
				existing = schemas.Key{ID: keyID}
			}
			if name, ok, err := optionalStringArg(args, "name"); err != nil {
				return nil, err
			} else if ok {
				existing.Name = name
			}
			if value, ok, err := optionalStringArg(args, "value"); err != nil {
				return nil, err
			} else if ok {
				existing.Value = *schemas.NewSecretVar(value)
			}
			if _, present := args["weight"]; present {
				weight, err := floatArg(args, "weight")
				if err != nil {
					return nil, err
				}
				existing.Weight = weight
			}
			if _, present := args["enabled"]; present {
				enabled, err := boolArg(args, "enabled")
				if err != nil {
					return nil, err
				}
				existing.Enabled = &enabled
			}
			if deps.ProviderRuntime != nil {
				if err := deps.ProviderRuntime.UpdateProviderKey(ctx, provider, keyID, existing); err != nil {
					return nil, fmt.Errorf("update provider key failed: %w", err)
				}
			} else if gov != nil {
				if err := gov.UpdateProviderKey(ctx, provider, keyID, existing); err != nil {
					return nil, fmt.Errorf("update provider key failed: %w", err)
				}
			}
			return map[string]any{"provider": providerName, "key": projectProviderKey(&existing)}, nil
		},
	}
}

func updateProviderTool() Tool {
	return Tool{
		name:        "update_provider",
		description: "Update a provider's concurrency, buffer size or description. Does not manage keys — use create_provider_key / update_provider_key.",
		schemaJSON: `{
  "type": "object",
  "properties": {
    "provider": {"type": "string", "minLength": 1},
    "concurrency": {"type": "integer", "minimum": 1, "description": "Worker pool size."},
    "buffer_size": {"type": "integer", "minimum": 1, "description": "Request queue buffer."},
    "description": {"type": "string"}
  },
  "required": ["provider"]
}`,
		noLogs:   true,
		mutating: true,
		execute: func(ctx context.Context, deps *Deps, args map[string]any) (any, error) {
			gov, err := requireGovernance(deps)
			if err != nil {
				return nil, err
			}
			name, err := stringArg(args, "provider")
			if err != nil {
				return nil, err
			}
			provider := schemas.ModelProvider(name)
			config, err := gov.GetProviderConfig(ctx, provider)
			if err != nil {
				if errors.Is(err, configstore.ErrNotFound) {
					return nil, fmt.Errorf("no provider named %q", name)
				}
				return nil, fmt.Errorf("get provider config failed: %w", err)
			}
			if config == nil {
				config = &configstore.ProviderConfig{}
			}
			concurrency, err := optionalIntArg(args, "concurrency")
			if err != nil {
				return nil, err
			}
			buffer, err := optionalIntArg(args, "buffer_size")
			if err != nil {
				return nil, err
			}
			if concurrency != nil || buffer != nil {
				current := schemas.DefaultConcurrencyAndBufferSize
				if config.ConcurrencyAndBufferSize != nil {
					current = *config.ConcurrencyAndBufferSize
				}
				if concurrency != nil {
					current.Concurrency = *concurrency
				}
				if buffer != nil {
					current.BufferSize = *buffer
				}
				config.ConcurrencyAndBufferSize = &current
			}
			if description, ok, err := optionalStringArg(args, "description"); err != nil {
				return nil, err
			} else if ok {
				config.Description = description
			}
			if deps.ProviderRuntime != nil {
				if err := deps.ProviderRuntime.UpdateProviderConfig(ctx, provider, *config); err != nil {
					return nil, fmt.Errorf("update provider failed: %w", err)
				}
			} else if err := gov.UpdateProvider(ctx, provider, *config); err != nil {
				return nil, fmt.Errorf("update provider failed: %w", err)
			}
			out := map[string]any{"provider": name, "description": config.Description}
			if config.ConcurrencyAndBufferSize != nil {
				out["concurrency"] = config.ConcurrencyAndBufferSize.Concurrency
				out["buffer_size"] = config.ConcurrencyAndBufferSize.BufferSize
			}
			return out, nil
		},
	}
}

func projectProviderKey(key *schemas.Key) map[string]any {
	out := map[string]any{
		"id":     key.ID,
		"name":   key.Name,
		"weight": key.Weight,
	}
	if key.Enabled != nil {
		out["enabled"] = *key.Enabled
	}
	if len(key.Models) > 0 {
		out["models"] = []string(key.Models)
	}
	if key.Status != "" {
		out["status"] = key.Status
	}
	return out
}
