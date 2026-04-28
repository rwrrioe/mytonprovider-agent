package jobs

import (
	"context"
	"encoding/json"
	"time"

	"github.com/rwrrioe/mytonprovider-agent/internal/domain"
)

// jobIDKey — ключ для проброса JobID через context.Value (consumer → handler → publisher).
type jobIDKey struct{}

func WithJobID(ctx context.Context, jobID string) context.Context {
	return context.WithValue(ctx, jobIDKey{}, jobID)
}

func JobIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(jobIDKey{}).(string)
	return v
}

const (
	CycleScanMaster       = "scan_master"
	CycleScanWallets      = "scan_wallets"
	CycleResolveEndpoints = "resolve_endpoints"
	CycleProbeRates       = "probe_rates"
	CycleInspectContracts = "inspect_contracts"
	CycleCheckProofs      = "check_proofs"
	CycleLookupIPInfo     = "lookup_ipinfo"
)

// TriggerEnvelope в mtpa:cycle:<type> бэк кладёт  агент читает
type TriggerEnvelope struct {
	JobID      string          `json:"job_id"`
	Type       string          `json:"type"`
	Hint       json.RawMessage `json:"hint,omitempty"`
	EnqueuedAt time.Time       `json:"enqueued_at"`
}

const (
	StatusOK    = "ok"
	StatusError = "error"
)

// ResultEnvelope mtpa:result:<type> агент кладет бэк чтитает
type ResultEnvelope struct {
	JobID       string    `json:"job_id"`
	Type        string    `json:"type"`
	Status      string    `json:"status"`
	Error       string    `json:"error,omitempty"`
	Payload     any       `json:"payload,omitempty"`
	ProcessedAt time.Time `json:"processed_at"`
	AgentID     string    `json:"agent_id"`
}

type ScanMasterResult struct {
	NewProviders []domain.Provider `json:"new_providers"`
	LastLT       uint64            `json:"last_lt"`
	ScannedCount int               `json:"scanned_count"`
}

type ScanWalletsResult struct {
	Contracts      []domain.StorageContract          `json:"contracts"`
	Relations      []domain.ContractProviderRelation `json:"relations"`
	UpdatedWallets []domain.Provider                 `json:"updated_wallets"`
}

type ResolveEndpointsResult struct {
	Endpoints []domain.ProviderEndpoint `json:"endpoints"`
	Skipped   int                       `json:"skipped"`
	Failed    int                       `json:"failed"`
}

type ProviderRateUpdate struct {
	PublicKey string       `json:"public_key"`
	Rates     domain.Rates `json:"rates"`
}

type ProbeRatesResult struct {
	Statuses []domain.ProviderStatus `json:"statuses"`
	Rates    []ProviderRateUpdate    `json:"rates"`
}

type InspectContractsResult struct {
	Rejected []domain.ContractProviderRelation `json:"rejected"`
	Skipped  []string                          `json:"skipped_addrs"`
}

type CheckProofsResult struct {
	Results []domain.ProofResult `json:"results"`
}

type IPInfoUpdate struct {
	PublicKey string        `json:"public_key"`
	IP        string        `json:"ip"`
	Info      domain.IpInfo `json:"info"`
}

type LookupIPInfoResult struct {
	Items []IPInfoUpdate `json:"items"`
}
