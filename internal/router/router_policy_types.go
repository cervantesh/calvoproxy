package router

import (
	"math"
	"net/http"
	"strings"
	"time"

	cervorules "github.com/cervantesh/cervo-rules/v3/core"
	cervolimits "github.com/cervantesh/cervo-rules/v3/limits"
	cervoruntime "github.com/cervantesh/cervo-rules/v3/runtime"
)

type RetryPolicy struct {
	MaxAttempts       int           `json:"max_attempts,omitempty"`
	RetryHTTPStatuses []int         `json:"retry_http_statuses,omitempty"`
	RetryOnEOF        bool          `json:"retry_on_eof,omitempty"`
	RetryOnTimeout    bool          `json:"retry_on_timeout,omitempty"`
	BackoffMin        time.Duration `json:"backoff_min,omitempty"`
	BackoffMax        time.Duration `json:"backoff_max,omitempty"`
}

type RetryAttempt struct {
	StatusCode int
	Retryable  bool
	Timeout    bool
	EOF        bool
}

type BreakerPolicy struct {
	FailureThreshold int           `json:"failure_threshold,omitempty"`
	Cooldown         time.Duration `json:"cooldown,omitempty"`
	Eligible         bool          `json:"eligible,omitempty"`
}

type Limits = cervolimits.Budget
type RequestedLimits = cervolimits.Requested
type LimitViolation = cervolimits.Violation
type LimitViolations = cervolimits.Violations

func ShouldRetry(policy RetryPolicy, attempt RetryAttempt) bool {
	if attempt.Timeout && policy.RetryOnTimeout {
		return true
	}
	if attempt.EOF && policy.RetryOnEOF {
		return true
	}
	if attempt.Retryable {
		return true
	}
	for _, status := range policy.RetryHTTPStatuses {
		if status == attempt.StatusCode {
			return true
		}
	}
	return false
}

func RetryBackoff(policy RetryPolicy, attemptIndex int) time.Duration {
	if attemptIndex <= 0 {
		return 0
	}
	min := policy.BackoffMin
	if min <= 0 {
		min = 100 * time.Millisecond
	}
	maxDelay := policy.BackoffMax
	if maxDelay <= 0 {
		maxDelay = min
	}
	delay := min * time.Duration(math.Pow(2, float64(attemptIndex-1)))
	if delay > maxDelay {
		return maxDelay
	}
	return delay
}

type requestFacts struct {
	ID            string
	User          string
	Channel       string
	Risk          string
	OperationHint cervorules.Operation
	Metadata      map[string]string
	Stream        bool
}

type policyDecision struct {
	Allow             bool
	Target            cervorules.Target
	Executor          cervorules.Executor
	FallbackExecutors []cervorules.Executor
	Reason            string
	RuleID            string
	AuditClass        string
	RequiresAudit     bool
	Timeout           time.Duration
	RetryPolicy       RetryPolicy
	BreakerPolicy     BreakerPolicy
	Limits            Limits
}

func (d policyDecision) coreDecision() cervorules.Decision {
	return cervorules.Decision{
		Allow:             d.Allow,
		Target:            d.Target,
		Executor:          d.Executor,
		FallbackExecutors: append([]cervorules.Executor(nil), d.FallbackExecutors...),
		Reason:            d.Reason,
	}
}

func defaultDeniedDecision(reason string) policyDecision {
	return policyDecision{Allow: false, Reason: reason, Timeout: defaultPolicyTimeout}
}

func defaultAllowedDecision() policyDecision {
	return policyDecision{
		Allow:         true,
		Target:        serviceCervoCore,
		Executor:      providerOpenRouter,
		Timeout:       defaultPolicyTimeout,
		RetryPolicy:   RetryPolicy{MaxAttempts: 1},
		BreakerPolicy: BreakerPolicy{FailureThreshold: 3, Cooldown: 60 * time.Second, Eligible: true},
	}
}

func requestedPolicyLimits(facts requestFacts, body []byte, requestedLimits ...RequestedLimits) RequestedLimits {
	requested := RequestedLimits{
		BodyBytes: int64(len(body)),
		Stream:    facts.Stream,
	}
	if len(requestedLimits) > 0 {
		requested = requestedLimits[0]
	}
	return requested
}

func validateDecisionLimits(w http.ResponseWriter, decision policyDecision, requested RequestedLimits) bool {
	violations := cervolimits.Check(decision.Limits, requested)
	if len(violations) == 0 {
		return true
	}
	for _, violation := range violations {
		if violation.Field == "body_bytes" {
			writeJSONError(w, http.StatusRequestEntityTooLarge, violation.Reason)
			return false
		}
	}
	writeJSONError(w, http.StatusForbidden, violations[0].Reason)
	return false
}

type ruleRuntimeConfig struct {
	cervoruntime.PolicyRuntimeConfig
	DefaultTimeout time.Duration
	RetryPolicy    RetryPolicy
	BreakerPolicy  BreakerPolicy
	Limits         Limits
}

func (cfg ruleRuntimeConfig) isTrustedUser(user string) bool {
	user = strings.TrimSpace(user)
	if user == "" {
		return false
	}
	for _, trusted := range cfg.TrustedUsers {
		if strings.EqualFold(strings.TrimSpace(trusted), user) {
			return true
		}
	}
	return false
}

func limitsForOperation(operation cervorules.Operation, override Limits) Limits {
	if hasCustomLimits(override) {
		return override
	}
	switch operation {
	case capChatCompletion:
		return Limits{AllowStream: true, AllowImages: true}
	case capToolOrchestration:
		return Limits{AllowTools: true}
	default:
		return Limits{}
	}
}

func auditClassForRequest(req cervorules.Request) string {
	if req.Operation == capSecretLookup {
		return "sensitive"
	}
	for _, value := range req.Metadata {
		lower := strings.ToLower(value)
		switch {
		case strings.Contains(lower, "secret"), strings.Contains(lower, "vault"), strings.Contains(lower, "credential"):
			return "sensitive"
		case strings.Contains(lower, "deploy"), strings.Contains(lower, "proxmox"):
			return "admin"
		}
	}
	return "none"
}
