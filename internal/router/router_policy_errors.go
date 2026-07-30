package router

import (
	"errors"
	"log/slog"

	cervorules "github.com/cervantesh/cervo-rules/v3/core"
	cervoruntime "github.com/cervantesh/cervo-rules/v3/runtime"
)

func policyErrorLogAttrs(err error) []slog.Attr {
	if err == nil {
		return nil
	}
	metadata := generatedPolicyMetadata()
	var buildErr *cervoruntime.PolicyBuildError
	if errors.As(err, &buildErr) {
		metadata = buildErr.Metadata
	}
	return newPolicyTelemetryEvent(cervorules.Request{}, policyDecision{}, metadata, err, 0).LogAttrs()
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
