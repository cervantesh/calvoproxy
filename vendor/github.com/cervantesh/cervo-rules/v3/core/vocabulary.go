package core

import "strconv"

// Vocabulary validates caller-owned names before a policy reaches runtime.
type Vocabulary interface {
	ValidateOperation(Operation) error
	ValidateTarget(Target) error
	ValidateExecutor(Executor) error
	ValidateRequest(Request) error
	ValidateDecision(Decision) error
}

// StaticVocabulary validates fixed operation, target, and executor sets.
//
// Empty name sets are intentionally open. This lets consumers validate only the
// dimensions they own centrally while keeping other dimensions flexible.
type StaticVocabulary struct {
	operations map[Operation]struct{}
	targets    map[Target]struct{}
	executors  map[Executor]struct{}
}

// VocabularyOption configures a Vocabulary.
type VocabularyOption func(*StaticVocabulary)

// NewVocabulary creates a vocabulary validator from option groups.
func NewVocabulary(options ...VocabularyOption) Vocabulary {
	v := StaticVocabulary{}
	for _, option := range options {
		if option != nil {
			option(&v)
		}
	}
	return v
}

// AllowedOperations restricts valid operation names.
func AllowedOperations(operations ...Operation) VocabularyOption {
	return func(v *StaticVocabulary) {
		v.operations = operationSet(operations)
	}
}

// AllowedTargets restricts valid target names.
func AllowedTargets(targets ...Target) VocabularyOption {
	return func(v *StaticVocabulary) {
		v.targets = targetSet(targets)
	}
}

// AllowedExecutors restricts valid executor names.
func AllowedExecutors(executors ...Executor) VocabularyOption {
	return func(v *StaticVocabulary) {
		v.executors = executorSet(executors)
	}
}

// ValidateOperation returns an error when an operation is not in the allowed set.
func (v StaticVocabulary) ValidateOperation(operation Operation) error {
	operation = NewOperation(string(operation))
	if operation == "" {
		return Error{
			Code:   ErrorCodeEmptyOperation,
			Field:  "operation",
			Reason: "operation is empty",
		}
	}
	if len(v.operations) == 0 {
		return nil
	}
	if _, ok := v.operations[operation]; !ok {
		return Error{
			Code:   ErrorCodeUnknownOperation,
			Field:  "operation",
			Value:  string(operation),
			Reason: "unknown operation is not in vocabulary",
		}
	}
	return nil
}

// ValidateTarget returns an error when a target is not in the allowed set.
func (v StaticVocabulary) ValidateTarget(target Target) error {
	target = NewTarget(string(target))
	if target == "" {
		return Error{
			Code:   ErrorCodeEmptyTarget,
			Field:  "target",
			Reason: "target is empty",
		}
	}
	if len(v.targets) == 0 {
		return nil
	}
	if _, ok := v.targets[target]; !ok {
		return Error{
			Code:   ErrorCodeUnknownTarget,
			Field:  "target",
			Value:  string(target),
			Reason: "unknown target is not in vocabulary",
		}
	}
	return nil
}

// ValidateExecutor returns an error when an executor is not in the allowed set.
func (v StaticVocabulary) ValidateExecutor(executor Executor) error {
	executor = NewExecutor(string(executor))
	if executor == "" {
		return Error{
			Code:   ErrorCodeEmptyExecutor,
			Field:  "executor",
			Reason: "executor is empty",
		}
	}
	if len(v.executors) == 0 {
		return nil
	}
	if _, ok := v.executors[executor]; !ok {
		return Error{
			Code:   ErrorCodeUnknownExecutor,
			Field:  "executor",
			Value:  string(executor),
			Reason: "unknown executor is not in vocabulary",
		}
	}
	return nil
}

// ValidateRequest validates vocabulary fields present on a request.
func (v StaticVocabulary) ValidateRequest(req Request) error {
	if req.Operation == "" {
		return nil
	}
	return v.ValidateOperation(req.Operation)
}

// ValidateDecision validates vocabulary fields present on a decision.
func (v StaticVocabulary) ValidateDecision(decision Decision) error {
	var errs Errors
	if decision.Target != "" {
		if err := v.ValidateTarget(decision.Target); err != nil {
			errs = appendValidationError(errs, err, "target")
		}
	}
	if decision.Executor != "" {
		if err := v.ValidateExecutor(decision.Executor); err != nil {
			errs = appendValidationError(errs, err, "executor")
		}
	}
	for i, target := range decision.FallbackTargets {
		if err := v.ValidateTarget(target); err != nil {
			errs = appendValidationError(errs, err, indexedField("fallback_targets", i))
		}
	}
	for i, executor := range decision.FallbackExecutors {
		if err := v.ValidateExecutor(executor); err != nil {
			errs = appendValidationError(errs, err, indexedField("fallback_executors", i))
		}
	}
	if len(errs) > 0 {
		return errs.SortStable()
	}
	return nil
}

func appendValidationError(errs Errors, err error, field string) Errors {
	switch typed := err.(type) {
	case Error:
		typed.Field = field
		return append(errs, typed)
	case Errors:
		for _, item := range typed {
			item.Field = field
			errs = append(errs, item)
		}
		return errs
	default:
		return append(errs, Error{
			Code:   ErrorCodeInvalidConfig,
			Field:  field,
			Reason: err.Error(),
		})
	}
}

func indexedField(name string, index int) string {
	return name + "[" + strconv.Itoa(index) + "]"
}

func operationSet(values []Operation) map[Operation]struct{} {
	out := map[Operation]struct{}{}
	for _, value := range values {
		if normalized := NewOperation(string(value)); normalized != "" {
			out[normalized] = struct{}{}
		}
	}
	return out
}

func targetSet(values []Target) map[Target]struct{} {
	out := map[Target]struct{}{}
	for _, value := range values {
		if normalized := NewTarget(string(value)); normalized != "" {
			out[normalized] = struct{}{}
		}
	}
	return out
}

func executorSet(values []Executor) map[Executor]struct{} {
	out := map[Executor]struct{}{}
	for _, value := range values {
		if normalized := NewExecutor(string(value)); normalized != "" {
			out[normalized] = struct{}{}
		}
	}
	return out
}
