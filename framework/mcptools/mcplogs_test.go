package mcptools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/framework/logstore"
	"github.com/stretchr/testify/require"
)

func TestQueryMCPLogsReportsUnavailableWithoutLogs(t *testing.T) {
	result := runToolViaHandler(t, "query_mcp_logs", &Deps{}, map[string]any{"filters": map[string]any{}})
	require.True(t, result.IsError)
	require.Contains(t, result.Content[0].(mcp.TextContent).Text, "logging is not enabled")
}

func TestQueryMCPLogsRejectsUnknownFilters(t *testing.T) {
	_, err := runTool(t, "query_mcp_logs", &Deps{LogManager: &fakeLogReader{}}, map[string]any{
		"filters": map[string]any{"providers": []any{"openai"}},
	})
	require.ErrorContains(t, err, "unknown filter fields")
}

func TestQueryMCPLogsProjectsCompactRows(t *testing.T) {
	latency := 12.5
	name := "prod"
	secret := "sk-should-never-appear"
	fake := &fakeLogReader{
		mcpSearchResult: &logstore.MCPToolLogSearchResult{Logs: []logstore.MCPToolLog{
			{
				ID:             "mcp-1",
				Timestamp:      time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC),
				ToolName:       "github.search",
				ServerLabel:    "github",
				Status:         "success",
				Latency:        &latency,
				VirtualKeyName: &name,
				Arguments:      `{"query":"` + secret + `"}`,
				Result:         `{"hits":1}`,
				VirtualKey:     &tables.TableVirtualKey{Value: schemas.SecretVar{Val: secret}},
			},
		}},
	}
	result, err := runTool(t, "query_mcp_logs", &Deps{LogManager: fake}, map[string]any{
		"filters": map[string]any{"start_time": "-1h"},
	})
	require.NoError(t, err)
	out := result.(map[string]any)
	rows := out["rows"].([]map[string]any)
	require.Len(t, rows, 1)
	require.Equal(t, "mcp-1", rows[0]["id"])
	require.Equal(t, "github.search", rows[0]["tool_name"])
	require.Equal(t, "/workspace/mcp-logs?selected_log=mcp-1", rows[0]["link"])
	require.Contains(t, out, "window")
	require.NotContains(t, rows[0], "arguments")
	require.NotContains(t, rows[0], "result")
	serialized := boundToolResult(result)
	require.NotContains(t, serialized, secret)
	require.NotNil(t, fake.mcpSearchFilters)
}

func TestGetMCPLogDetailIncludesTruncatedPayload(t *testing.T) {
	payload := strings.Repeat("a", DetailContentChars+50)
	fake := &fakeLogReader{
		mcpLog: &logstore.MCPToolLog{
			ID:        "mcp-1",
			Timestamp: time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC),
			ToolName:  "echo",
			Status:    "success",
			Arguments: payload,
			Result:    "ok",
		},
	}
	result, err := runTool(t, "get_mcp_log_detail", &Deps{LogManager: fake}, map[string]any{"log_id": "mcp-1"})
	require.NoError(t, err)
	out := result.(map[string]any)
	require.Equal(t, "ok", out["result"])
	args, ok := out["arguments"].(string)
	require.True(t, ok)
	require.True(t, strings.HasSuffix(args, "... [truncated]"))
	require.LessOrEqual(t, len(args), DetailContentChars+len("... [truncated]"))
}

func TestGetMCPLogDetailPassesCallerContext(t *testing.T) {
	type scopeKey struct{}
	fake := &fakeLogReader{
		mcpLog: &logstore.MCPToolLog{ID: "mcp-1", Timestamp: time.Now(), ToolName: "echo", Status: "success"},
	}
	ctx := context.WithValue(context.Background(), scopeKey{}, "caller-scope")
	_, err := runToolCtx(t, ctx, "get_mcp_log_detail", &Deps{LogManager: fake}, map[string]any{"log_id": "mcp-1"})
	require.NoError(t, err)
	require.Equal(t, "caller-scope", fake.sawContext.Value(scopeKey{}))
}

func TestCountMCPLogsGuidesOnEmpty(t *testing.T) {
	fake := &fakeLogReader{mcpStats: &logstore.MCPToolLogStats{}}
	result, err := runTool(t, "count_mcp_logs", &Deps{LogManager: fake}, map[string]any{
		"filters": map[string]any{},
	})
	require.NoError(t, err)
	out := result.(map[string]any)
	require.EqualValues(t, 0, out["total_executions"])
	require.Contains(t, out["guidance"], "Nothing matched")
	require.Contains(t, out, "window")
}

func TestCountMCPLogsSaysWhenTooManyToList(t *testing.T) {
	fake := &fakeLogReader{mcpStats: &logstore.MCPToolLogStats{TotalExecutions: LargeResultThreshold + 1}}
	result, err := runTool(t, "count_mcp_logs", &Deps{LogManager: fake}, map[string]any{
		"filters": map[string]any{},
	})
	require.NoError(t, err)
	out := result.(map[string]any)
	require.Equal(t, true, out["too_many_to_list"])
	require.Contains(t, out["guidance"], "too many to list")
}

func TestQueryMCPMetricsSummary(t *testing.T) {
	fake := &fakeLogReader{mcpStats: &logstore.MCPToolLogStats{TotalExecutions: 4, SuccessRate: 75}}
	result, err := runTool(t, "query_mcp_metrics", &Deps{LogManager: fake}, map[string]any{
		"filters": map[string]any{"start_time": "-1h"},
		"metrics": []any{"summary"},
	})
	require.NoError(t, err)
	out := result.(map[string]any)
	require.Equal(t, int64(4), out["summary"].(*logstore.MCPToolLogStats).TotalExecutions)
	require.NotContains(t, out, "volume", "only the metrics asked for are fetched")
	require.Contains(t, out, "window")
}

func TestQueryMCPMetricsReturnsVolumeAndCost(t *testing.T) {
	fake := &fakeLogReader{
		mcpHistogram:     &logstore.MCPHistogramResult{BucketSizeSeconds: 60},
		mcpCostHistogram: &logstore.MCPCostHistogramResult{BucketSizeSeconds: 60},
	}
	result, err := runTool(t, "query_mcp_metrics", &Deps{LogManager: fake}, map[string]any{
		"filters": map[string]any{"start_time": "-1h"},
		"metrics": []any{"volume", "cost"},
	})
	require.NoError(t, err)
	out := result.(map[string]any)
	require.Same(t, fake.mcpHistogram, out["volume"])
	require.Same(t, fake.mcpCostHistogram, out["cost"])
	require.NotContains(t, out, "summary")
}

func TestQueryMCPMetricsRequiresMetrics(t *testing.T) {
	_, err := runTool(t, "query_mcp_metrics", &Deps{LogManager: &fakeLogReader{}}, map[string]any{
		"filters": map[string]any{"start_time": "-1h"},
	})
	require.Error(t, err)
}

func TestQueryMCPUsageByFlattensTools(t *testing.T) {
	fake := &fakeLogReader{
		mcpTopTools: &logstore.MCPTopToolsResult{
			Tools: []logstore.MCPTopToolResult{{ToolName: "echo", Count: 3, Cost: 0.01}},
		},
	}
	result, err := runTool(t, "query_mcp_usage_by", &Deps{LogManager: fake}, map[string]any{
		"filters": map[string]any{"start_time": "-1h"},
	})
	require.NoError(t, err)
	tools := result.(map[string]any)["tools"].([]logstore.MCPTopToolResult)
	require.Len(t, tools, 1)
	require.Equal(t, "echo", tools[0].ToolName)
}
