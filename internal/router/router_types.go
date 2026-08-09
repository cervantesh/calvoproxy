package router

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	cervomodelpolicy "github.com/cervantesh/cervo-model-policy"
	cervorules "github.com/cervantesh/cervo-rules/v3/core"
	cervoruntime "github.com/cervantesh/cervo-rules/v3/runtime"
)

type breakerConfig struct {
	FailureThreshold int
	Cooldown         time.Duration
	RequestTimeout   time.Duration // per-attempt timeout (one upstream call)
	TotalTimeout     time.Duration // overall wall-clock budget across the chain
}

type modelBreakerState struct {
	ConsecutiveFailures int       `json:"consecutive_failures"`
	Successes           int       `json:"successes"`
	LastFailureCode     int       `json:"last_failure_code,omitempty"`
	LastFailureReason   string    `json:"last_failure_reason,omitempty"`
	LastFailureAt       time.Time `json:"last_failure_at,omitempty"`
	OpenUntil           time.Time `json:"open_until,omitempty"`
	// ProbeUntil single-flights the half-open recovery probe: when a circuit's
	// cooldown elapses, exactly one caller claims the probe (setting this to a
	// short TTL) and the rest are skipped until the probe resolves or the TTL
	// expires, instead of every concurrent request stampeding the recovering
	// model at once.
	ProbeUntil     time.Time `json:"-"`
	Score          float64   `json:"score"`
	ScoreUpdatedAt time.Time `json:"score_updated_at,omitempty"`
	// ScoreAttemptSeq is the proxy-wide scored-attempt counter as of the last
	// score update. Decay is measured against how much evidence has accumulated
	// since — not only how much time has passed — so an idle proxy does not
	// quietly forget which of its models actually work.
	ScoreAttemptSeq int64 `json:"score_attempt_seq,omitempty"`
}

type BreakerSnapshot struct {
	Model               string    `json:"model"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	Successes           int       `json:"successes"`
	LastFailureCode     int       `json:"last_failure_code,omitempty"`
	LastFailureReason   string    `json:"last_failure_reason,omitempty"`
	LastFailureAt       time.Time `json:"last_failure_at,omitempty"`
	OpenUntil           time.Time `json:"open_until,omitempty"`
	Score               float64   `json:"score"`
	State               string    `json:"state"`
}

type ProviderSnapshot struct {
	Provider            string                `json:"provider"`
	Configured          bool                  `json:"configured"`
	State               string                `json:"state"`
	Attempts            int64                 `json:"attempts"`
	ReliabilityScore    float64               `json:"reliability_score"`
	ConsecutiveFailures int                   `json:"consecutive_failures"`
	LastFailureCode     int                   `json:"last_failure_code,omitempty"`
	LastFailureReason   string                `json:"last_failure_reason,omitempty"`
	LastFailureAt       time.Time             `json:"last_failure_at,omitempty"`
	OpenUntil           time.Time             `json:"open_until,omitempty"`
	Quota               []QuotaHealthSnapshot `json:"quota,omitempty"`
}

type QuotaHealthSnapshot struct {
	Scope       string          `json:"scope"`
	ModelOrPool string          `json:"model_or_pool"`
	Dimension   QuotaDimension  `json:"dimension"`
	Window      QuotaWindow     `json:"window"`
	Limit       int64           `json:"limit"`
	Remaining   int64           `json:"remaining"`
	Reserved    int64           `json:"reserved"`
	Available   int64           `json:"available"`
	ResetAt     time.Time       `json:"reset_at,omitempty"`
	Source      QuotaSource     `json:"source"`
	Confidence  QuotaConfidence `json:"confidence"`
}

type ProxyHealth struct {
	Service            string             `json:"service"`
	Status             string             `json:"status"`
	Ready              bool               `json:"ready"`
	OpenCircuitCount   int                `json:"open_circuit_count"`
	ConfiguredAPIKey   bool               `json:"configured_api_key"`
	DefaultExecutor    string             `json:"default_executor"`
	Profiles           []string           `json:"profiles"`
	FailureThreshold   int                `json:"failure_threshold"`
	CooldownSeconds    int                `json:"cooldown_seconds"`
	RequestTimeoutSecs int                `json:"request_timeout_seconds"`
	PolicyName         string             `json:"policy_name"`
	PolicyDSLVersion   string             `json:"policy_dsl_version"`
	PolicyHash         string             `json:"policy_hash"`
	PolicyVocabHash    string             `json:"policy_vocabulary_hash"`
	ModelPolicy        ModelPolicyHealth  `json:"model_policy"`
	Providers          []ProviderSnapshot `json:"providers"`
	Circuits           []BreakerSnapshot  `json:"circuits"`
	Quotas             []QuotaSnapshot    `json:"quotas,omitempty"`
	Timestamp          time.Time          `json:"timestamp"`
}

type ModelPolicyHealth struct {
	DefaultProfile     string                             `json:"default_profile"`
	Profiles           []string                           `json:"profiles"`
	Aliases            []string                           `json:"aliases"`
	Strict             bool                               `json:"strict"`
	ValidationWarnings []cervomodelpolicy.ValidationIssue `json:"validation_warnings,omitempty"`
}

type attemptError struct {
	StatusCode      int
	Message         string
	BreakerEligible bool
	Retryable       bool
	Timeout         bool
	EOF             bool
	// SkipModel marks a soft, model-specific skip (e.g. a half-open probe already
	// in flight): the fallback loop advances to the next model immediately with no
	// backoff and no score/breaker penalty, exactly like a model-unavailable 404.
	SkipModel bool
	// ProviderUnavailable means the failure applies to the provider/account,
	// not just this model. The fallback loop skips sibling models from the same
	// provider while preserving this original actionable error.
	ProviderUnavailable bool
	// QuotaLimited is deliberately separate from reliability/breaker failure.
	// A provider can be healthy while one request/token window is exhausted.
	QuotaLimited bool
	// QuotaPool identifies a shared quota pool (currently OpenRouter's free
	// models) without declaring the entire provider unavailable.
	QuotaPool  string
	RetryAfter time.Duration
}

func (e *attemptError) Error() string { return e.Message }

type RouterService struct {
	Client           HTTPDoer
	SideEffects      SideEffectExtractor
	Transformer      ResponseTransformer
	AttemptPlanner   ModelAttemptPlanner
	TargetResolver   AttemptTargetResolver
	Fallbacks        FallbackExecutor
	PolicyEngine     cervorules.Engine
	config           breakerConfig
	policyMu         sync.RWMutex // guards policy + modelPolicy for hot-reload
	policy           policyConfig
	providerProfiles providerProfiles
	contextWindows   contextWindowIndex
	modelPolicy      *cervomodelpolicy.Policy
	modelWarnings    []cervomodelpolicy.ValidationIssue
	modelStrict      bool
	runtimeConfig    ruleRuntimeConfig
	policyMetadata   cervoruntime.PolicyMetadata
	breakerMu        sync.RWMutex
	modelBreakers    map[string]*modelBreakerState
	providerBreakers map[providerID]*modelBreakerState
	// providerAttempts is the durable global consumption clock used to balance
	// which healthy provider starts each request. It counts real upstream calls
	// (plus the primary reservation made atomically by the scheduler).
	providerAttempts map[providerID]int64
	// providerBalance is a virtual fair-scheduling clock. It is separate from
	// providerAttempts so rebasing a newly recovered/configured provider avoids
	// catch-up bursts without falsifying the actual usage table.
	providerBalance map[providerID]int64
	quotaInitMu     sync.Mutex
	quota           *QuotaLedger
	quotaCooldownMu sync.RWMutex
	quotaCooldowns  map[string]time.Time
	affinity        *affinityStore
	// scoreAttempts counts every scored outcome, proxy-wide. It is the "evidence
	// clock" score decay is measured against (see router_scoring.go).
	scoreAttempts atomic.Int64
	// scoresDirty marks the map as diverged from the persisted file;
	// persistScores marks that a flusher is running and Close() should do a
	// final write. See router_scoring_store.go.
	scoresDirty   atomic.Bool
	persistScores atomic.Bool
	admission     *admissionControl
	capabilities  *capabilityIndex
	traceRingOnce sync.Once
	traces        *traceRing
	counters      routerCounters
	cancelRefresh context.CancelFunc
	// AmbientKeyPresent, when set by the binary, reports whether an ambient
	// upstream key is configured from ANY source (env or the `calvoproxy login`
	// file). The router only knows about the env var, so /health used to claim no
	// key was configured for a login-file user.
	AmbientKeyPresent func() bool
	// AmbientProviderCredential resolves a provider credential at the moment an
	// upstream attempt is made. Keeping this as a callback avoids caching vault
	// plaintext in the router and makes key replacement effective immediately.
	// The returned byte slice remains owned by the caller and is cleared after
	// it has been copied into the HTTP request.
	AmbientProviderCredential func(provider string) ([]byte, bool)
}

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type SideEffectExtractor interface {
	Extract(context.Context, string) map[string]any
}

type ResponseTransformer interface {
	Transform(context.Context, []byte) []byte
}

type ModelAttemptPlanner interface {
	Plan(policyDecision, string, string) []modelAttempt
}

type AttemptTargetResolver interface {
	Resolve(modelAttempt, string) AttemptTarget
}

type AttemptExecutor interface {
	ExecuteAttempt(context.Context, http.ResponseWriter, []byte, string, modelAttempt) error
}

type FallbackExecutor interface {
	Execute(context.Context, http.ResponseWriter, FallbackExecution) error
}

type FallbackExecution struct {
	RequestBody map[string]interface{}
	APIKey      string
	Attempts    []modelAttempt
	RetryPolicy RetryPolicy
	// Stream marks a streamed (SSE) request: such attempts are not wrapped in a
	// per-attempt deadline (streaming is bounded by header + idle timeouts
	// instead), so a long-but-live stream is never cut mid-response.
	Stream bool
	// PerAttemptTimeout bounds a single non-streaming upstream call; zero leaves
	// the attempt bounded only by the parent context.
	PerAttemptTimeout time.Duration
	// ReserveQuota atomically claims quota for a fallback at the instant a
	// streaming attempt reaches its first-event fail-fast boundary.
	ReserveQuota func(modelAttempt) (QuotaTicket, bool)
	// OnSuccess observes the route that actually produced the response. It runs
	// only after a complete non-stream response or a cleanly completed stream.
	OnSuccess func(modelAttempt)
	// OnFailure lets soft affinity release a route as soon as it fails. A later
	// successful fallback then becomes the new preference.
	OnFailure func(modelAttempt)
}

type AttemptTarget struct {
	URL     string
	Agentic bool
}

type modelAttempt struct {
	Profile       string
	Model         string
	Provider      cervorules.Executor
	BreakerPolicy BreakerPolicy
	// Path is the upstream operation path (empty ⇒ chat completions). Set to the
	// messages path to route an Anthropic-compatible request through the same
	// model chain / breaker / scoring / fallback machinery as chat.
	Path string
	// AttemptIndex is this attempt's 1-based position in the chain, surfaced to
	// the client as X-Calvoproxy-Attempt. Anything above 1 means the request was
	// served by a fallback — the single most useful thing a caller can know and
	// the one thing an HTTP status never tells it.
	AttemptIndex int
	// LastInChain marks the final attempt the executor will make. The
	// fail-fast first-event budget is skipped for it: with nothing left to fall
	// back to, abandoning would turn a slow success into a fast failure —
	// exactly backwards during the broad upstream slowness where an answer
	// matters most. The last attempt runs under the ordinary bounds.
	LastInChain bool
	// ReserveFallback runs only when the first-event budget expires. It closes
	// the availability race without holding unused provider quota throughout a
	// healthy primary stream.
	ReserveFallback func() bool
	// BalanceReserved means the scheduler already reserved this primary in the
	// virtual fair-use clock. The real upstream attempt is still counted once.
	BalanceReserved bool
	// QuotaReservations are atomically claimed by the scheduler for the primary
	// attempt. Fallback attempts claim their own immediately before dispatch.
	QuotaTicket   QuotaTicket
	QuotaEstimate QuotaEstimate
}
