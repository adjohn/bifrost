package mcptools

import (
	"context"
	"fmt"

	"github.com/maximhq/bifrost/framework/configstore/tables"
)

func getWarpConfigTool() Tool {
	return Tool{
		name:        "get_warp_config",
		description: "Warp dashboard-agent settings: enabled, model, embedding, semantic-search knobs. Never returns credentials — only key ids.",
		schemaJSON: `{
  "type": "object",
  "properties": {}
}`,
		noLogs: true,
		execute: func(ctx context.Context, deps *Deps, args map[string]any) (any, error) {
			if deps.Warp == nil {
				return nil, fmt.Errorf("warp configuration is not available on this deployment")
			}
			row, err := deps.Warp.GetWarpConfig(ctx)
			if err != nil {
				return nil, fmt.Errorf("get warp config failed: %w", err)
			}
			if row == nil {
				return map[string]any{"configured": false}, nil
			}
			return projectWarpConfig(row), nil
		},
	}
}

func updateWarpConfigTool() Tool {
	return Tool{
		name:        "update_warp_config",
		description: "Update Warp settings. api_key_id and embedding_api_key_id are references to configured provider keys, not secrets.",
		schemaJSON: `{
  "type": "object",
  "properties": {
    "enabled": {"type": "boolean"},
    "provider": {"type": "string"},
    "model": {"type": "string"},
    "api_key_id": {"type": "string"},
    "base_url": {"type": "string"},
    "max_iterations": {"type": "integer", "minimum": 1},
    "request_timeout_seconds": {"type": "integer", "minimum": 1},
    "embedding_provider": {"type": "string"},
    "embedding_model": {"type": "string"},
    "embedding_api_key_id": {"type": "string"},
    "semantic_search_threshold": {"type": "number"},
    "semantic_search_limit": {"type": "integer", "minimum": 1}
  }
}`,
		noLogs:   true,
		mutating: true,
		execute: func(ctx context.Context, deps *Deps, args map[string]any) (any, error) {
			if deps.Warp == nil {
				return nil, fmt.Errorf("warp configuration is not available on this deployment")
			}
			row, err := deps.Warp.GetWarpConfig(ctx)
			if err != nil {
				return nil, fmt.Errorf("get warp config failed: %w", err)
			}
			if row == nil {
				row = &tables.TableWarpConfig{}
			}
			if flag, err := optionalBoolArg(args, "enabled"); err != nil {
				return nil, err
			} else if flag != nil {
				row.Enabled = *flag
			}
			if v, ok, err := optionalStringArg(args, "provider"); err != nil {
				return nil, err
			} else if ok {
				row.Provider = v
			}
			if v, ok, err := optionalStringArg(args, "model"); err != nil {
				return nil, err
			} else if ok {
				row.Model = v
			}
			if v, ok, err := optionalStringArg(args, "api_key_id"); err != nil {
				return nil, err
			} else if ok {
				row.APIKeyID = v
			}
			if v, ok, err := optionalStringArg(args, "base_url"); err != nil {
				return nil, err
			} else if ok {
				row.BaseURL = v
			}
			if n, err := optionalIntArg(args, "max_iterations"); err != nil {
				return nil, err
			} else if n != nil {
				row.MaxIterations = *n
			}
			if n, err := optionalIntArg(args, "request_timeout_seconds"); err != nil {
				return nil, err
			} else if n != nil {
				row.RequestTimeoutSeconds = *n
			}
			if v, ok, err := optionalStringArg(args, "embedding_provider"); err != nil {
				return nil, err
			} else if ok {
				row.EmbeddingProvider = v
			}
			if v, ok, err := optionalStringArg(args, "embedding_model"); err != nil {
				return nil, err
			} else if ok {
				row.EmbeddingModel = v
			}
			if v, ok, err := optionalStringArg(args, "embedding_api_key_id"); err != nil {
				return nil, err
			} else if ok {
				row.EmbeddingAPIKeyID = v
			}
			if _, present := args["semantic_search_threshold"]; present {
				n, err := floatArg(args, "semantic_search_threshold")
				if err != nil {
					return nil, err
				}
				row.SemanticSearchThreshold = n
			}
			if n, err := optionalIntArg(args, "semantic_search_limit"); err != nil {
				return nil, err
			} else if n != nil {
				row.SemanticSearchLimit = *n
			}
			if err := deps.Warp.UpsertWarpConfig(ctx, row); err != nil {
				return nil, fmt.Errorf("update warp config failed: %w", err)
			}
			return projectWarpConfig(row), nil
		},
	}
}

func projectWarpConfig(row *tables.TableWarpConfig) map[string]any {
	out := map[string]any{
		"configured":                true,
		"enabled":                   row.Enabled,
		"provider":                  row.Provider,
		"model":                     row.Model,
		"max_iterations":            row.MaxIterations,
		"request_timeout_seconds":   row.RequestTimeoutSeconds,
		"embedding_provider":        row.EmbeddingProvider,
		"embedding_model":           row.EmbeddingModel,
		"semantic_search_threshold": row.SemanticSearchThreshold,
		"semantic_search_limit":     row.SemanticSearchLimit,
	}
	if row.APIKeyID != "" {
		out["api_key_id"] = row.APIKeyID
	}
	if row.EmbeddingAPIKeyID != "" {
		out["embedding_api_key_id"] = row.EmbeddingAPIKeyID
	}
	if row.BaseURL != "" {
		out["base_url"] = row.BaseURL
	}
	return out
}
