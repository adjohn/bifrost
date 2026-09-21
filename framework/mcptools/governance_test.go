package mcptools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/framework/logstore"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// GovernanceReaderStub embeds the GovernanceReader interface without implementing
// it, so a fake that only overrides the methods it needs panics on anything new.
type GovernanceReaderStub struct {
	GovernanceReader
}

// fakeGovernanceReader is describe_virtual_key's one dependency. lookup is
// keyed on id, mirroring GetVirtualKey's own "not found" contract: a missing
// key returns configstore.ErrNotFound, not a nil, nil pair.
type fakeGovernanceReader struct {
	GovernanceReaderStub
	byID         map[string]*tables.TableVirtualKey
	teams        map[string]*tables.TableTeam
	customers    map[string]*tables.TableCustomer
	budgets      map[string]*tables.TableBudget
	providers    []tables.TableProvider
	providerKeys map[schemas.ModelProvider][]schemas.Key
	providerCfgs map[schemas.ModelProvider]configstore.ProviderConfig
	mcpClients   map[string]*tables.TableMCPClient
	virtualMCPs  map[uint]*tables.TableVirtualMCP
	vmcpVKs      map[uint][]string
	routing      map[string]*tables.TableRoutingRule
	rateLimits   map[string]*tables.TableRateLimit
	modelConfigs map[string]*tables.TableModelConfig
	plugins      map[string]*tables.TablePlugin
	pricing      map[string]*tables.TablePricingOverride
	webhooks     map[string]*tables.TableWebhookEndpoint
	flags        map[string]tables.TableFeatureFlag
	oauthTokens  []tables.TableMCPOauthToken
	headerCreds  []tables.TableMCPPerUserHeaderCredential
	clientConfig *configstore.ClientConfig
	err          error
	sawContext   context.Context
	sawID        string
	createdVK    *tables.TableVirtualKey
	addedMCP     *schemas.MCPClientConfig
}

func (f *fakeGovernanceReader) GetVirtualKey(ctx context.Context, id string) (*tables.TableVirtualKey, error) {
	f.sawContext = ctx
	f.sawID = id
	if f.err != nil {
		return nil, f.err
	}
	vk, ok := f.byID[id]
	if !ok {
		return nil, configstore.ErrNotFound
	}
	return vk, nil
}

func TestDescribeVirtualKeyReportsUnavailableWithoutAGovernanceReader(t *testing.T) {
	_, err := runTool(t, "describe_virtual_key", &Deps{}, map[string]any{"virtual_key_id": "vk-1"})
	require.ErrorContains(t, err, "not available")
}

func TestDescribeVirtualKeyRequiresAnID(t *testing.T) {
	deps := &Deps{Governance: &fakeGovernanceReader{}}
	_, err := runTool(t, "describe_virtual_key", deps, map[string]any{"virtual_key_id": "  "})
	require.ErrorContains(t, err, "virtual_key_id")
}

func TestDescribeVirtualKeyReportsUnknownID(t *testing.T) {
	fake := &fakeGovernanceReader{byID: map[string]*tables.TableVirtualKey{}}
	deps := &Deps{Governance: fake}
	_, err := runTool(t, "describe_virtual_key", deps, map[string]any{"virtual_key_id": "vk-missing"})
	require.ErrorContains(t, err, "vk-missing")
	require.ErrorContains(t, err, "describe_filter_space")
}

// The caller's context is what carries queryscope's row-level filter into the
// store - GetVirtualKey narrows to rows the caller may see the same way every
// LogReader method does. Losing it here would return any key to anyone who
// asked, the same failure mode LogReader's own tools guard against.
func TestDescribeVirtualKeyPassesCallerContextToStore(t *testing.T) {
	type scopeKey struct{}
	fake := &fakeGovernanceReader{byID: map[string]*tables.TableVirtualKey{
		"vk-1": {ID: "vk-1", Name: "prod"},
	}}
	deps := &Deps{Governance: fake}
	ctx := context.WithValue(context.Background(), scopeKey{}, "caller-scope")
	_, err := runToolCtx(t, ctx, "describe_virtual_key", deps, map[string]any{"virtual_key_id": "vk-1"})
	require.NoError(t, err)
	require.Equal(t, "caller-scope", fake.sawContext.Value(scopeKey{}))
	require.Equal(t, "vk-1", fake.sawID)
}

// The result must never carry the key's own secret value, its rotation
// history, or any provider credential beneath it - only the budget/limit/
// provider shape describeVirtualKey hand-picks. This is the regression test
// for that: a row deliberately carrying secret-shaped data in every field
// describeVirtualKey does not touch, asserting none of it survives.
func TestDescribeVirtualKeyNeverLeaksSecretFields(t *testing.T) {
	teamID := "team-1"
	expires := time.Now().Add(24 * time.Hour)
	vk := &tables.TableVirtualKey{
		ID:                "vk-1",
		Name:              "prod",
		Description:       "production traffic",
		TeamID:            &teamID,
		ExpiresAt:         &expires,
		Value:             schemas.SecretVar{Val: "sk-super-secret-value"},
		PreviousValueHash: "leftover-hash",
		Budgets: []tables.TableBudget{
			{ID: "budget-1", MaxLimit: 100, CurrentUsage: 42, ResetDuration: "1M", LastReset: time.Now()},
		},
		RateLimit: &tables.TableRateLimit{ID: "rl-1", TokenMaxLimit: int64Ptr(1000), TokenCurrentUsage: 250},
		ProviderConfigs: []tables.TableVirtualKeyProviderConfig{
			{
				Provider:      "openai",
				AllowedModels: []string{"gpt-4o"},
				Keys: []tables.TableKey{
					{ID: 1, Name: "prod-openai-key", Value: schemas.SecretVar{Val: "sk-should-never-appear"}},
				},
			},
		},
	}
	fake := &fakeGovernanceReader{byID: map[string]*tables.TableVirtualKey{"vk-1": vk}}
	deps := &Deps{Governance: fake}

	result, err := runTool(t, "describe_virtual_key", deps, map[string]any{"virtual_key_id": "vk-1"})
	require.NoError(t, err)

	out, ok := result.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "vk-1", out["id"])
	require.Equal(t, "prod", out["name"])
	require.Equal(t, "team-1", out["team_id"])

	budgets, ok := out["budgets"].([]map[string]any)
	require.True(t, ok)
	require.Len(t, budgets, 1)
	require.InDelta(t, 100.0, budgets[0]["max_limit"], 0.001)
	require.InDelta(t, 42.0, budgets[0]["current_usage"], 0.001)

	rateLimit, ok := out["rate_limit"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, int64(1000), rateLimit["token_max_limit"])
	require.NotContains(t, rateLimit, "request_max_limit", "an unset limit family must not read as a limit of zero")

	providers, ok := out["providers"].([]map[string]any)
	require.True(t, ok)
	require.Len(t, providers, 1)
	require.Equal(t, "openai", providers[0]["provider"])
	require.Equal(t, []string{"gpt-4o"}, providers[0]["allowed_models"])
	require.NotContains(t, providers[0], "keys", "no provider key detail, secret or otherwise, may reach the model")

	// The bounded-result serialization is the last line of defense; walking
	// the returned value directly is what proves the secret was never placed
	// there in the first place, not merely stripped afterward.
	serialized := boundToolResult(result)
	require.NotContains(t, serialized, "sk-super-secret-value")
	require.NotContains(t, serialized, "sk-should-never-appear")
	require.NotContains(t, serialized, "leftover-hash")
	require.NotContains(t, serialized, "prod-openai-key", "not even a key's name belongs in a chat tool result")
}

// A budget under an active override must report the effective cap, not the
// raw one the override has already changed - the same distinction the
// dashboard itself makes (see TableBudget.EffectiveMaxLimit).
func TestDescribeVirtualKeyBudgetReportsEffectiveLimitUnderOverride(t *testing.T) {
	vk := &tables.TableVirtualKey{
		ID:   "vk-1",
		Name: "prod",
		Budgets: []tables.TableBudget{
			{
				ID: "budget-1", MaxLimit: 100, CurrentUsage: 10, ResetDuration: "1M", LastReset: time.Now(),
				OverrideAmount: 50, OverrideMode: tables.BudgetOverrideModeForever,
			},
		},
	}
	fake := &fakeGovernanceReader{byID: map[string]*tables.TableVirtualKey{"vk-1": vk}}
	deps := &Deps{Governance: fake}

	result, err := runTool(t, "describe_virtual_key", deps, map[string]any{"virtual_key_id": "vk-1"})
	require.NoError(t, err)

	budgets := result.(map[string]any)["budgets"].([]map[string]any)
	require.InDelta(t, 150.0, budgets[0]["max_limit"], 0.001, "override amount must be folded into the reported cap")
	require.Equal(t, true, budgets[0]["override_active"])
}

func int64Ptr(v int64) *int64 { return &v }

func (f *fakeGovernanceReader) GetVirtualKeysPaginated(ctx context.Context, params configstore.VirtualKeyQueryParams) ([]tables.TableVirtualKey, int64, error) {
	f.sawContext = ctx
	out := make([]tables.TableVirtualKey, 0, len(f.byID))
	for _, vk := range f.byID {
		out = append(out, *vk)
	}
	return out, int64(len(out)), nil
}

func (f *fakeGovernanceReader) CreateVirtualKey(ctx context.Context, virtualKey *tables.TableVirtualKey, tx ...*gorm.DB) error {
	f.sawContext = ctx
	if f.byID == nil {
		f.byID = map[string]*tables.TableVirtualKey{}
	}
	f.byID[virtualKey.ID] = virtualKey
	f.createdVK = virtualKey
	return nil
}

func (f *fakeGovernanceReader) UpdateVirtualKey(ctx context.Context, virtualKey *tables.TableVirtualKey, tx ...*gorm.DB) error {
	f.sawContext = ctx
	if f.byID == nil {
		f.byID = map[string]*tables.TableVirtualKey{}
	}
	f.byID[virtualKey.ID] = virtualKey
	return nil
}

func (f *fakeGovernanceReader) GetTeam(ctx context.Context, id string) (*tables.TableTeam, error) {
	if f.teams == nil {
		return nil, configstore.ErrNotFound
	}
	team, ok := f.teams[id]
	if !ok {
		return nil, configstore.ErrNotFound
	}
	return team, nil
}

func (f *fakeGovernanceReader) GetTeamsPaginated(ctx context.Context, params configstore.TeamsQueryParams) ([]tables.TableTeam, int64, error) {
	out := make([]tables.TableTeam, 0, len(f.teams))
	for _, team := range f.teams {
		out = append(out, *team)
	}
	return out, int64(len(out)), nil
}

func (f *fakeGovernanceReader) CreateTeam(ctx context.Context, team *tables.TableTeam, tx ...*gorm.DB) error {
	if f.teams == nil {
		f.teams = map[string]*tables.TableTeam{}
	}
	f.teams[team.ID] = team
	return nil
}

func (f *fakeGovernanceReader) UpdateTeam(ctx context.Context, team *tables.TableTeam, tx ...*gorm.DB) error {
	if f.teams == nil {
		f.teams = map[string]*tables.TableTeam{}
	}
	f.teams[team.ID] = team
	return nil
}

func (f *fakeGovernanceReader) GetCustomer(ctx context.Context, id string) (*tables.TableCustomer, error) {
	if f.customers == nil {
		return nil, configstore.ErrNotFound
	}
	customer, ok := f.customers[id]
	if !ok {
		return nil, configstore.ErrNotFound
	}
	return customer, nil
}

func (f *fakeGovernanceReader) GetCustomersPaginated(ctx context.Context, params configstore.CustomersQueryParams) ([]tables.TableCustomer, int64, error) {
	out := make([]tables.TableCustomer, 0, len(f.customers))
	for _, customer := range f.customers {
		out = append(out, *customer)
	}
	return out, int64(len(out)), nil
}

func (f *fakeGovernanceReader) CreateCustomer(ctx context.Context, customer *tables.TableCustomer, tx ...*gorm.DB) error {
	if f.customers == nil {
		f.customers = map[string]*tables.TableCustomer{}
	}
	f.customers[customer.ID] = customer
	return nil
}

func (f *fakeGovernanceReader) UpdateCustomer(ctx context.Context, customer *tables.TableCustomer, tx ...*gorm.DB) error {
	if f.customers == nil {
		f.customers = map[string]*tables.TableCustomer{}
	}
	f.customers[customer.ID] = customer
	return nil
}

func (f *fakeGovernanceReader) GetBudget(ctx context.Context, id string, tx ...*gorm.DB) (*tables.TableBudget, error) {
	if f.budgets == nil {
		return nil, configstore.ErrNotFound
	}
	budget, ok := f.budgets[id]
	if !ok {
		return nil, configstore.ErrNotFound
	}
	return budget, nil
}

func (f *fakeGovernanceReader) GetBudgets(ctx context.Context) ([]tables.TableBudget, error) {
	out := make([]tables.TableBudget, 0, len(f.budgets))
	for _, budget := range f.budgets {
		out = append(out, *budget)
	}
	return out, nil
}

func (f *fakeGovernanceReader) CreateBudget(ctx context.Context, budget *tables.TableBudget, tx ...*gorm.DB) error {
	if f.budgets == nil {
		f.budgets = map[string]*tables.TableBudget{}
	}
	f.budgets[budget.ID] = budget
	return nil
}

func (f *fakeGovernanceReader) UpdateBudget(ctx context.Context, budget *tables.TableBudget, tx ...*gorm.DB) error {
	if f.budgets == nil {
		f.budgets = map[string]*tables.TableBudget{}
	}
	f.budgets[budget.ID] = budget
	return nil
}

func (f *fakeGovernanceReader) GetProviders(ctx context.Context) ([]tables.TableProvider, error) {
	return f.providers, nil
}

func (f *fakeGovernanceReader) GetProviderKeys(ctx context.Context, provider schemas.ModelProvider) ([]schemas.Key, error) {
	if f.providerKeys == nil {
		return nil, configstore.ErrNotFound
	}
	keys, ok := f.providerKeys[provider]
	if !ok {
		return nil, configstore.ErrNotFound
	}
	return keys, nil
}

func (f *fakeGovernanceReader) AddProvider(ctx context.Context, provider schemas.ModelProvider, config configstore.ProviderConfig, tx ...*gorm.DB) error {
	f.providers = append(f.providers, tables.TableProvider{Name: string(provider)})
	return nil
}

func (f *fakeGovernanceReader) CreateProviderKey(ctx context.Context, provider schemas.ModelProvider, key schemas.Key, tx ...*gorm.DB) error {
	if f.providerKeys == nil {
		f.providerKeys = map[schemas.ModelProvider][]schemas.Key{}
	}
	f.providerKeys[provider] = append(f.providerKeys[provider], key)
	return nil
}

func (f *fakeGovernanceReader) UpdateProviderKey(ctx context.Context, provider schemas.ModelProvider, keyID string, key schemas.Key, tx ...*gorm.DB) error {
	keys := f.providerKeys[provider]
	for i := range keys {
		if keys[i].ID == keyID {
			keys[i] = key
			f.providerKeys[provider] = keys
			return nil
		}
	}
	return configstore.ErrNotFound
}

func (f *fakeGovernanceReader) GetProviderConfig(ctx context.Context, provider schemas.ModelProvider) (*configstore.ProviderConfig, error) {
	if f.providerCfgs == nil {
		return nil, configstore.ErrNotFound
	}
	cfg, ok := f.providerCfgs[provider]
	if !ok {
		return nil, configstore.ErrNotFound
	}
	return &cfg, nil
}

func (f *fakeGovernanceReader) UpdateProvider(ctx context.Context, provider schemas.ModelProvider, config configstore.ProviderConfig, tx ...*gorm.DB) error {
	if f.providerCfgs == nil {
		f.providerCfgs = map[schemas.ModelProvider]configstore.ProviderConfig{}
	}
	f.providerCfgs[provider] = config
	return nil
}

func (f *fakeGovernanceReader) GetMCPClientByID(ctx context.Context, id string) (*tables.TableMCPClient, error) {
	if f.mcpClients == nil {
		return nil, configstore.ErrNotFound
	}
	row, ok := f.mcpClients[id]
	if !ok {
		return nil, configstore.ErrNotFound
	}
	return row, nil
}

func (f *fakeGovernanceReader) GetMCPClientsPaginated(ctx context.Context, params configstore.MCPClientsQueryParams) ([]tables.TableMCPClient, int64, error) {
	out := make([]tables.TableMCPClient, 0, len(f.mcpClients))
	for _, row := range f.mcpClients {
		out = append(out, *row)
	}
	return out, int64(len(out)), nil
}

func (f *fakeGovernanceReader) CreateMCPClientConfig(ctx context.Context, clientConfig *schemas.MCPClientConfig) error {
	f.addedMCP = clientConfig
	if f.mcpClients == nil {
		f.mcpClients = map[string]*tables.TableMCPClient{}
	}
	f.mcpClients[clientConfig.ID] = &tables.TableMCPClient{
		ClientID:       clientConfig.ID,
		Name:           clientConfig.Name,
		ConnectionType: string(clientConfig.ConnectionType),
	}
	return nil
}

func (f *fakeGovernanceReader) UpdateMCPClientConfig(ctx context.Context, id string, clientConfig *tables.TableMCPClient) error {
	if f.mcpClients == nil {
		f.mcpClients = map[string]*tables.TableMCPClient{}
	}
	f.mcpClients[id] = clientConfig
	return nil
}

func (f *fakeGovernanceReader) GetVirtualMCPByID(ctx context.Context, id uint) (*tables.TableVirtualMCP, error) {
	if f.virtualMCPs == nil {
		return nil, configstore.ErrNotFound
	}
	row, ok := f.virtualMCPs[id]
	if !ok {
		return nil, configstore.ErrNotFound
	}
	return row, nil
}

func (f *fakeGovernanceReader) GetVirtualMCPsPaginated(ctx context.Context, params configstore.VirtualMCPsQueryParams) ([]tables.TableVirtualMCP, int64, error) {
	out := make([]tables.TableVirtualMCP, 0, len(f.virtualMCPs))
	for _, row := range f.virtualMCPs {
		out = append(out, *row)
	}
	return out, int64(len(out)), nil
}

func (f *fakeGovernanceReader) CreateVirtualMCP(ctx context.Context, def *tables.TableVirtualMCP) error {
	if f.virtualMCPs == nil {
		f.virtualMCPs = map[uint]*tables.TableVirtualMCP{}
	}
	if def.ID == 0 {
		def.ID = uint(len(f.virtualMCPs) + 1)
	}
	f.virtualMCPs[def.ID] = def
	return nil
}

func (f *fakeGovernanceReader) UpdateVirtualMCP(ctx context.Context, def *tables.TableVirtualMCP) error {
	if f.virtualMCPs == nil {
		f.virtualMCPs = map[uint]*tables.TableVirtualMCP{}
	}
	f.virtualMCPs[def.ID] = def
	return nil
}

func (f *fakeGovernanceReader) AttachVirtualMCPToVirtualKey(ctx context.Context, vmcpID uint, virtualKeyID string) error {
	if f.vmcpVKs == nil {
		f.vmcpVKs = map[uint][]string{}
	}
	f.vmcpVKs[vmcpID] = append(f.vmcpVKs[vmcpID], virtualKeyID)
	return nil
}

func (f *fakeGovernanceReader) DetachVirtualMCPFromVirtualKey(ctx context.Context, vmcpID uint, virtualKeyID string) error {
	kept := f.vmcpVKs[vmcpID][:0]
	for _, id := range f.vmcpVKs[vmcpID] {
		if id != virtualKeyID {
			kept = append(kept, id)
		}
	}
	f.vmcpVKs[vmcpID] = kept
	return nil
}

func (f *fakeGovernanceReader) GetVirtualKeyIDsForVirtualMCP(ctx context.Context, vmcpID uint) ([]string, error) {
	return f.vmcpVKs[vmcpID], nil
}

func (f *fakeGovernanceReader) GetClientConfig(ctx context.Context) (*configstore.ClientConfig, error) {
	if f.clientConfig == nil {
		return &configstore.ClientConfig{}, nil
	}
	return f.clientConfig, nil
}

func (f *fakeGovernanceReader) GetRoutingRule(ctx context.Context, id string) (*tables.TableRoutingRule, error) {
	if f.routing == nil {
		return nil, configstore.ErrNotFound
	}
	row, ok := f.routing[id]
	if !ok {
		return nil, configstore.ErrNotFound
	}
	return row, nil
}

func (f *fakeGovernanceReader) GetRoutingRulesPaginated(ctx context.Context, params configstore.RoutingRulesQueryParams) ([]tables.TableRoutingRule, int64, error) {
	out := make([]tables.TableRoutingRule, 0, len(f.routing))
	for _, row := range f.routing {
		out = append(out, *row)
	}
	return out, int64(len(out)), nil
}

func (f *fakeGovernanceReader) CreateRoutingRule(ctx context.Context, rule *tables.TableRoutingRule, tx ...*gorm.DB) error {
	if f.routing == nil {
		f.routing = map[string]*tables.TableRoutingRule{}
	}
	f.routing[rule.ID] = rule
	return nil
}

func (f *fakeGovernanceReader) UpdateRoutingRule(ctx context.Context, rule *tables.TableRoutingRule, tx ...*gorm.DB) error {
	if f.routing == nil {
		f.routing = map[string]*tables.TableRoutingRule{}
	}
	f.routing[rule.ID] = rule
	return nil
}

func (f *fakeGovernanceReader) GetRateLimit(ctx context.Context, id string, tx ...*gorm.DB) (*tables.TableRateLimit, error) {
	if f.rateLimits == nil {
		return nil, configstore.ErrNotFound
	}
	row, ok := f.rateLimits[id]
	if !ok {
		return nil, configstore.ErrNotFound
	}
	return row, nil
}

func (f *fakeGovernanceReader) GetRateLimits(ctx context.Context) ([]tables.TableRateLimit, error) {
	out := make([]tables.TableRateLimit, 0, len(f.rateLimits))
	for _, row := range f.rateLimits {
		out = append(out, *row)
	}
	return out, nil
}

func (f *fakeGovernanceReader) CreateRateLimit(ctx context.Context, rateLimit *tables.TableRateLimit, tx ...*gorm.DB) error {
	if f.rateLimits == nil {
		f.rateLimits = map[string]*tables.TableRateLimit{}
	}
	f.rateLimits[rateLimit.ID] = rateLimit
	return nil
}

func (f *fakeGovernanceReader) UpdateRateLimit(ctx context.Context, rateLimit *tables.TableRateLimit, tx ...*gorm.DB) error {
	if f.rateLimits == nil {
		f.rateLimits = map[string]*tables.TableRateLimit{}
	}
	f.rateLimits[rateLimit.ID] = rateLimit
	return nil
}

func (f *fakeGovernanceReader) GetModelConfigByID(ctx context.Context, id string) (*tables.TableModelConfig, error) {
	if f.modelConfigs == nil {
		return nil, configstore.ErrNotFound
	}
	row, ok := f.modelConfigs[id]
	if !ok {
		return nil, configstore.ErrNotFound
	}
	return row, nil
}

func (f *fakeGovernanceReader) GetModelConfigsPaginated(ctx context.Context, params configstore.ModelConfigsQueryParams) ([]tables.TableModelConfig, int64, error) {
	out := make([]tables.TableModelConfig, 0, len(f.modelConfigs))
	for _, row := range f.modelConfigs {
		out = append(out, *row)
	}
	return out, int64(len(out)), nil
}

func (f *fakeGovernanceReader) CreateModelConfig(ctx context.Context, modelConfig *tables.TableModelConfig, tx ...*gorm.DB) error {
	if f.modelConfigs == nil {
		f.modelConfigs = map[string]*tables.TableModelConfig{}
	}
	f.modelConfigs[modelConfig.ID] = modelConfig
	return nil
}

func (f *fakeGovernanceReader) UpdateModelConfig(ctx context.Context, modelConfig *tables.TableModelConfig, tx ...*gorm.DB) error {
	if f.modelConfigs == nil {
		f.modelConfigs = map[string]*tables.TableModelConfig{}
	}
	f.modelConfigs[modelConfig.ID] = modelConfig
	return nil
}

func (f *fakeGovernanceReader) GetPlugin(ctx context.Context, name string) (*tables.TablePlugin, error) {
	if f.plugins == nil {
		return nil, configstore.ErrNotFound
	}
	row, ok := f.plugins[name]
	if !ok {
		return nil, configstore.ErrNotFound
	}
	return row, nil
}

func (f *fakeGovernanceReader) GetPlugins(ctx context.Context) ([]*tables.TablePlugin, error) {
	out := make([]*tables.TablePlugin, 0, len(f.plugins))
	for _, row := range f.plugins {
		out = append(out, row)
	}
	return out, nil
}

func (f *fakeGovernanceReader) CreatePlugin(ctx context.Context, plugin *tables.TablePlugin, tx ...*gorm.DB) error {
	if f.plugins == nil {
		f.plugins = map[string]*tables.TablePlugin{}
	}
	f.plugins[plugin.Name] = plugin
	return nil
}

func (f *fakeGovernanceReader) UpdatePlugin(ctx context.Context, plugin *tables.TablePlugin, tx ...*gorm.DB) error {
	if f.plugins == nil {
		f.plugins = map[string]*tables.TablePlugin{}
	}
	f.plugins[plugin.Name] = plugin
	return nil
}

func (f *fakeGovernanceReader) GetPricingOverrideByID(ctx context.Context, id string) (*tables.TablePricingOverride, error) {
	if f.pricing == nil {
		return nil, configstore.ErrNotFound
	}
	row, ok := f.pricing[id]
	if !ok {
		return nil, configstore.ErrNotFound
	}
	return row, nil
}

func (f *fakeGovernanceReader) GetPricingOverridesPaginated(ctx context.Context, params configstore.PricingOverridesQueryParams) ([]tables.TablePricingOverride, int64, error) {
	out := make([]tables.TablePricingOverride, 0, len(f.pricing))
	for _, row := range f.pricing {
		out = append(out, *row)
	}
	return out, int64(len(out)), nil
}

func (f *fakeGovernanceReader) CreatePricingOverride(ctx context.Context, override *tables.TablePricingOverride, tx ...*gorm.DB) error {
	if f.pricing == nil {
		f.pricing = map[string]*tables.TablePricingOverride{}
	}
	f.pricing[override.ID] = override
	return nil
}

func (f *fakeGovernanceReader) UpdatePricingOverride(ctx context.Context, override *tables.TablePricingOverride, tx ...*gorm.DB) error {
	if f.pricing == nil {
		f.pricing = map[string]*tables.TablePricingOverride{}
	}
	f.pricing[override.ID] = override
	return nil
}

func (f *fakeGovernanceReader) GetWebhookEndpointByID(ctx context.Context, id string) (*tables.TableWebhookEndpoint, error) {
	if f.webhooks == nil {
		return nil, configstore.ErrNotFound
	}
	row, ok := f.webhooks[id]
	if !ok {
		return nil, configstore.ErrNotFound
	}
	return row, nil
}

func (f *fakeGovernanceReader) GetWebhookEndpointsPaginated(ctx context.Context, params configstore.WebhookEndpointsQueryParams) ([]tables.TableWebhookEndpoint, int64, error) {
	out := make([]tables.TableWebhookEndpoint, 0, len(f.webhooks))
	for _, row := range f.webhooks {
		out = append(out, *row)
	}
	return out, int64(len(out)), nil
}

func (f *fakeGovernanceReader) CreateWebhookEndpoint(ctx context.Context, endpoint *tables.TableWebhookEndpoint) error {
	if f.webhooks == nil {
		f.webhooks = map[string]*tables.TableWebhookEndpoint{}
	}
	if endpoint.Secret == nil || endpoint.Secret.GetValue() == "" {
		endpoint.Secret = schemas.NewSecretVar("whsec_test")
	}
	f.webhooks[endpoint.ID] = endpoint
	return nil
}

func (f *fakeGovernanceReader) UpdateWebhookEndpoint(ctx context.Context, endpoint *tables.TableWebhookEndpoint) error {
	if f.webhooks == nil {
		f.webhooks = map[string]*tables.TableWebhookEndpoint{}
	}
	f.webhooks[endpoint.ID] = endpoint
	return nil
}

func (f *fakeGovernanceReader) ListFeatureFlags(ctx context.Context) ([]tables.TableFeatureFlag, error) {
	out := make([]tables.TableFeatureFlag, 0, len(f.flags))
	for _, row := range f.flags {
		out = append(out, row)
	}
	return out, nil
}

func (f *fakeGovernanceReader) UpsertFeatureFlag(ctx context.Context, id string, enabled bool, updatedAt int64) error {
	if f.flags == nil {
		f.flags = map[string]tables.TableFeatureFlag{}
	}
	f.flags[id] = tables.TableFeatureFlag{ID: id, Enabled: enabled, UpdatedAt: updatedAt}
	return nil
}

func (f *fakeGovernanceReader) ListOauthUserTokens(ctx context.Context, params configstore.MCPSessionsFilterParams) ([]tables.TableMCPOauthToken, error) {
	return f.oauthTokens, nil
}

func (f *fakeGovernanceReader) ListMCPPerUserHeaderCredentials(ctx context.Context, params configstore.MCPSessionsFilterParams) ([]tables.TableMCPPerUserHeaderCredential, error) {
	return f.headerCreds, nil
}

func TestCreateVirtualKeyReturnsSecretOnceAndListDoesNot(t *testing.T) {
	fake := &fakeGovernanceReader{byID: map[string]*tables.TableVirtualKey{}}
	deps := &Deps{Governance: fake}

	created, err := runTool(t, "create_virtual_key", deps, map[string]any{"name": "ops"})
	require.NoError(t, err)
	out := created.(map[string]any)
	secret, _ := out["value"].(string)
	require.True(t, strings.HasPrefix(secret, VirtualKeyPrefix))
	require.Contains(t, boundToolResult(created), secret)

	listed, err := runTool(t, "list_virtual_keys", deps, map[string]any{})
	require.NoError(t, err)
	require.NotContains(t, boundToolResult(listed), secret)
}

func TestRotateVirtualKeyReturnsNewSecret(t *testing.T) {
	active := true
	vk := &tables.TableVirtualKey{
		ID:       "vk-1",
		Name:     "ops",
		Value:    *schemas.NewSecretVar("sk-bf-old"),
		IsActive: &active,
	}
	fake := &fakeGovernanceReader{byID: map[string]*tables.TableVirtualKey{"vk-1": vk}}
	deps := &Deps{Governance: fake}

	result, err := runTool(t, "rotate_virtual_key", deps, map[string]any{"virtual_key_id": "vk-1"})
	require.NoError(t, err)
	out := result.(map[string]any)
	secret, _ := out["value"].(string)
	require.True(t, strings.HasPrefix(secret, VirtualKeyPrefix))
	require.NotEqual(t, "sk-bf-old", secret)
}

func TestCreateBudgetRequiresExactlyOneOwner(t *testing.T) {
	fake := &fakeGovernanceReader{}
	deps := &Deps{Governance: fake}
	_, err := runTool(t, "create_budget", deps, map[string]any{"max_limit": 10.0, "reset_duration": "1d"})
	require.ErrorContains(t, err, "team_id or customer_id")
}

func TestStdioMCPClientIsRefused(t *testing.T) {
	fake := &fakeGovernanceReader{}
	deps := &Deps{Governance: fake}
	_, err := runTool(t, "add_mcp_client", deps, map[string]any{
		"name": "shell", "connection_type": "stdio", "connection_string": "npx foo",
	})
	require.ErrorContains(t, err, "stdio")
}

func TestAddMCPClientRefusesReservedName(t *testing.T) {
	fake := &fakeGovernanceReader{}
	deps := &Deps{Governance: fake}
	_, err := runTool(t, "add_mcp_client", deps, map[string]any{
		"name": "bifrostmcp", "connection_type": "http", "connection_string": "https://example.com",
	})
	require.ErrorContains(t, err, "reserved")
}

func TestListProviderKeysNeverReturnsSecret(t *testing.T) {
	fake := &fakeGovernanceReader{providerKeys: map[schemas.ModelProvider][]schemas.Key{
		"openai": {{ID: "k1", Name: "prod", Value: *schemas.NewSecretVar("sk-never-leak")}},
	}}
	deps := &Deps{Governance: fake}
	result, err := runTool(t, "list_provider_keys", deps, map[string]any{"provider": "openai"})
	require.NoError(t, err)
	require.NotContains(t, boundToolResult(result), "sk-never-leak")
}

func TestGetHealthSkipsPingsWhenDisabled(t *testing.T) {
	result, err := runTool(t, "get_health", &Deps{DisableDBPings: true}, map[string]any{})
	require.NoError(t, err)
	out := result.(map[string]any)
	require.Equal(t, "ok", out["status"])
	require.Equal(t, "disabled", out["components"].(map[string]string)["config_store"])
}

func TestGetVersionReportsUnknownWhenUnset(t *testing.T) {
	result, err := runTool(t, "get_version", &Deps{}, map[string]any{})
	require.NoError(t, err)
	require.Equal(t, "unknown", result.(map[string]any)["version"])
}

func TestGetSessionReportsUnknownID(t *testing.T) {
	_, err := runTool(t, "get_session", &Deps{LogManager: &fakeLogReader{}}, map[string]any{"session_id": "missing"})
	require.ErrorContains(t, err, "missing")
}

func TestQueryMCPLogsReturnsProjectedRows(t *testing.T) {
	fake := &fakeLogReader{mcpSearchResult: &logstore.MCPToolLogSearchResult{
		Logs: []logstore.MCPToolLog{{ID: "m1", ToolName: "echo", Status: "success"}},
	}}
	result, err := runTool(t, "query_mcp_logs", &Deps{LogManager: fake}, map[string]any{"filters": map[string]any{}})
	require.NoError(t, err)
	rows := result.(map[string]any)["rows"].([]map[string]any)
	require.Equal(t, "m1", rows[0]["id"])
	require.Equal(t, "echo", rows[0]["tool_name"])
}

func TestCreateVirtualMCPAndAttach(t *testing.T) {
	fake := &fakeGovernanceReader{}
	deps := &Deps{Governance: fake}
	created, err := runTool(t, "create_virtual_mcp", deps, map[string]any{"name": "ops-bundle"})
	require.NoError(t, err)
	id := created.(map[string]any)["id"].(uint)
	_, err = runTool(t, "attach_virtual_mcp", deps, map[string]any{"virtual_mcp_id": float64(id), "virtual_key_id": "vk-1"})
	require.NoError(t, err)
	require.Equal(t, []string{"vk-1"}, fake.vmcpVKs[id])
}

func TestDeleteToolsAreNotDeclared(t *testing.T) {
	for _, name := range []string{
		"delete_provider_key",
		"delete_mcp_client",
		"delete_virtual_mcp",
		"delete_routing_rule",
		"delete_rate_limit",
		"delete_model_config",
		"delete_plugin",
		"delete_pricing_override",
		"delete_webhook",
		"delete_virtual_key",
		"delete_team",
		"delete_customer",
	} {
		_, ok := toolByName(buildTools(), name)
		require.False(t, ok, "%s must not be hosted", name)
	}
}

func TestCreateRoutingRuleRequiresWeightsSumToOne(t *testing.T) {
	fake := &fakeGovernanceReader{}
	deps := &Deps{Governance: fake}
	_, err := runTool(t, "create_routing_rule", deps, map[string]any{
		"name": "split",
		"targets": []any{
			map[string]any{"provider": "openai", "weight": 0.5},
			map[string]any{"provider": "anthropic", "weight": 0.3},
		},
	})
	require.ErrorContains(t, err, "sum to 1")
}

func TestCreateRoutingRulePersistsTargets(t *testing.T) {
	fake := &fakeGovernanceReader{}
	deps := &Deps{Governance: fake}
	result, err := runTool(t, "create_routing_rule", deps, map[string]any{
		"name":           "prod-split",
		"cel_expression": `provider == "openai"`,
		"targets": []any{
			map[string]any{"provider": "openai", "model": "gpt-4o", "weight": 1.0},
		},
	})
	require.NoError(t, err)
	out := result.(map[string]any)
	require.Equal(t, "prod-split", out["name"])
	require.Equal(t, "global", out["scope"])
	require.Len(t, fake.routing, 1)
}

func TestCreateRateLimitAttachesToTeam(t *testing.T) {
	fake := &fakeGovernanceReader{teams: map[string]*tables.TableTeam{
		"team-1": {ID: "team-1", Name: "ops"},
	}}
	deps := &Deps{Governance: fake}
	result, err := runTool(t, "create_rate_limit", deps, map[string]any{
		"owner_type":             "team",
		"owner_id":               "team-1",
		"request_max_limit":      float64(100),
		"request_reset_duration": "1h",
	})
	require.NoError(t, err)
	out := result.(map[string]any)
	require.Equal(t, "team", out["owner_type"])
	require.Equal(t, int64(100), out["request_max_limit"])
	require.NotEmpty(t, fake.teams["team-1"].RateLimitID)
}

func TestCreateModelConfigGlobal(t *testing.T) {
	fake := &fakeGovernanceReader{}
	deps := &Deps{Governance: fake}
	result, err := runTool(t, "create_model_config", deps, map[string]any{
		"model_name":           "gpt-4o",
		"provider":             "openai",
		"max_limit":            50.0,
		"reset_duration":       "1d",
		"token_max_limit":      float64(1000),
		"token_reset_duration": "1h",
	})
	require.NoError(t, err)
	out := result.(map[string]any)
	require.Equal(t, "gpt-4o", out["model_name"])
	require.Equal(t, "global", out["scope"])
	require.Len(t, fake.modelConfigs, 1)
}

func TestListMCPAuthSessionsOmitsTokens(t *testing.T) {
	fake := &fakeGovernanceReader{
		oauthTokens: []tables.TableMCPOauthToken{{
			ID:           "tok-1",
			AuthMode:     "user",
			MCPClientID:  "client-1",
			Status:       "active",
			AccessToken:  "secret-access",
			RefreshToken: "secret-refresh",
		}},
	}
	deps := &Deps{Governance: fake}
	result, err := runTool(t, "list_mcp_auth_sessions", deps, map[string]any{})
	require.NoError(t, err)
	serialized := boundToolResult(result)
	require.NotContains(t, serialized, "secret-access")
	require.NotContains(t, serialized, "secret-refresh")
	rows := result.(map[string]any)["sessions"].([]map[string]any)
	require.Equal(t, "oauth", rows[0]["kind"])
	require.Equal(t, true, rows[0]["has_refresh"])
}

func TestGetWarpConfigUnavailableWithoutStore(t *testing.T) {
	_, err := runTool(t, "get_warp_config", &Deps{}, map[string]any{})
	require.ErrorContains(t, err, "not available")
}

func TestUpdateWarpConfigPersists(t *testing.T) {
	store := &fakeWarpStore{}
	deps := &Deps{Warp: store}
	result, err := runTool(t, "update_warp_config", deps, map[string]any{
		"enabled":  true,
		"provider": "openai",
		"model":    "gpt-4o",
	})
	require.NoError(t, err)
	out := result.(map[string]any)
	require.Equal(t, true, out["enabled"])
	require.Equal(t, "openai", store.row.Provider)
}

func TestListWebhooksNeverReturnsSecret(t *testing.T) {
	secret := schemas.NewSecretVar("whsec_must_not_leak")
	fake := &fakeGovernanceReader{webhooks: map[string]*tables.TableWebhookEndpoint{
		"wh-1": {ID: "wh-1", Name: "alerts", URL: "https://example.com/hook", Secret: secret, Events: []tables.WebhookEvent{tables.WebhookEventAsyncJobCompleted}},
	}}
	result, err := runTool(t, "list_webhooks", &Deps{Governance: fake}, map[string]any{})
	require.NoError(t, err)
	require.NotContains(t, boundToolResult(result), "whsec_must_not_leak")
}

type fakeWarpStore struct {
	row *tables.TableWarpConfig
}

func (f *fakeWarpStore) GetWarpConfig(context.Context) (*tables.TableWarpConfig, error) {
	return f.row, nil
}

func (f *fakeWarpStore) UpsertWarpConfig(_ context.Context, config *tables.TableWarpConfig) error {
	copied := *config
	f.row = &copied
	return nil
}
