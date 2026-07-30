package router

import (
	"context"
	"net/http"
	"sync"
	"time"

	cervomodelpolicy "github.com/cervantesh/cervo-model-policy"
	cervorules "github.com/cervantesh/cervo-rules/v3/core"
	cervoruntime "github.com/cervantesh/cervo-rules/v3/runtime"
)

type breakerConfig struct {
	FailureThreshold int
	Cooldown         time.Duration
	RequestTimeout   time.Duration
}

type modelBreakerState struct {
	ConsecutiveFailures int       `json:"consecutive_failures"`
	Successes           int       `json:"successes"`
	LastFailureCode     int       `json:"last_failure_code,omitempty"`
	LastFailureReason   string    `json:"last_failure_reason,omitempty"`
	LastFailureAt       time.Time `json:"last_failure_at,omitempty"`
	OpenUntil           time.Time `json:"open_until,omitempty"`
	Score               float64   `json:"score"`
	ScoreUpdatedAt      time.Time `json:"score_updated_at,omitempty"`
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

type ProxyHealth struct {
	Service            string            `json:"service"`
	Status             string            `json:"status"`
	Ready              bool              `json:"ready"`
	OpenCircuitCount   int               `json:"open_circuit_count"`
	ConfiguredAPIKey   bool              `json:"configured_api_key"`
	DefaultExecutor    string            `json:"default_executor"`
	Profiles           []string          `json:"profiles"`
	FailureThreshold   int               `json:"failure_threshold"`
	CooldownSeconds    int               `json:"cooldown_seconds"`
	RequestTimeoutSecs int               `json:"request_timeout_seconds"`
	PolicyName         string            `json:"policy_name"`
	PolicyDSLVersion   string            `json:"policy_dsl_version"`
	PolicyHash         string            `json:"policy_hash"`
	PolicyVocabHash    string            `json:"policy_vocabulary_hash"`
	ModelPolicy        ModelPolicyHealth `json:"model_policy"`
	Circuits           []BreakerSnapshot `json:"circuits"`
	Timestamp          time.Time         `json:"timestamp"`
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
}

func (e *attemptError) Error() string { return e.Message }

type RouterService struct {
	Client         HTTPDoer
	SideEffects    SideEffectExtractor
	Transformer    ResponseTransformer
	AttemptPlanner ModelAttemptPlanner
	TargetResolver AttemptTargetResolver
	Fallbacks      FallbackExecutor
	PolicyEngine   cervorules.Engine
	config         breakerConfig
	policy         policyConfig
	modelPolicy    *cervomodelpolicy.Policy
	modelWarnings  []cervomodelpolicy.ValidationIssue
	modelStrict    bool
	runtimeConfig  ruleRuntimeConfig
	policyMetadata cervoruntime.PolicyMetadata
	breakerMu      sync.RWMutex
	modelBreakers  map[string]*modelBreakerState
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
}
