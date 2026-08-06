package router

import (
	"net/http"
	"strings"

	policyvocab "github.com/cervantesh/calvoproxy/internal/router/policyvocab"
	cervorules "github.com/cervantesh/cervo-rules/v3/core"
	cervohttpadapter "github.com/cervantesh/cervo-rules/v3/httpadapter"
)

type httpClassificationOptions = cervohttpadapter.HTTPClassificationOptions
type httpHeaderOptions = cervohttpadapter.HeaderOptions
type pathOperation = cervohttpadapter.PathOperation

type httpClassifier struct {
	inner *cervohttpadapter.Classifier
}

func newHTTPClassifier(options httpClassificationOptions) (*httpClassifier, error) {
	inner, err := cervohttpadapter.NewClassifier(options)
	if err != nil {
		return nil, err
	}
	return &httpClassifier{inner: inner}, nil
}

func (c *httpClassifier) FactsFromHTTPRequest(r *http.Request) requestFacts {
	if c == nil || r == nil {
		return requestFacts{}
	}
	httpFacts := c.inner.FactsFromHTTPRequest(r)
	return requestFacts{
		ID: httpFacts.ID,
		// The caller's asserted identity, and ONLY when this deployment has said
		// it will believe one. See userFromHeader.
		User:          userFromHeader(httpFacts.User),
		Channel:       httpFacts.Channel,
		Risk:          httpFacts.Risk,
		OperationHint: httpFacts.Operation,
		Metadata:      map[string]string{},
	}
}

// userFromHeader decides whether to believe the identity a request claims for
// itself.
//
// The policy can gate a route on `requires_trusted_user`, and it resolves that
// against the user in the request facts. That user arrives in a header the
// caller sets — so the gate was satisfied by asserting the right name, not by
// proving it. Measured: a plain chat request carrying `X-Cervo-User: <a name in
// the policy's trusted list>` authorised the `secret_lookup` route.
//
// The blast radius today is narrow: the policy's Target is an audit label and
// does not choose an upstream (that comes from the model attempt's provider), so
// nothing was redirected. What was corrupted is the audit record — it would
// report an authorised secret_lookup by a trusted user for what was an ordinary,
// unauthenticated chat request. That record is the thing this policy layer
// exists to produce, so the gate has to actually gate.
//
// Default: the header is not believed, so `requires_trusted_user` routes deny.
// PROXY_TRUST_USER_HEADER=true restores the old behaviour for deployments that
// front the proxy with something that authenticates the caller and sets this
// header itself. That is a real topology; it is just not a safe default.
func userFromHeader(user string) string {
	if user == "" || !trustUserHeader() {
		return ""
	}
	return user
}

func trustUserHeader() bool {
	return strings.EqualFold(strings.TrimSpace(envValue("PROXY_TRUST_USER_HEADER")), "true")
}

// validateClientOperation reports whether an operation the CALLER named is one
// the vocabulary declares.
//
// `cervohttpadapter` builds the operation with `core.NewOperation`, which
// normalises a string and validates nothing: any value of `X-Cervo-Capability`
// becomes an operation, and it beats the operation the proxy derived from the
// path. An unknown one already failed — with "no route for operation" — but by
// accident, because no route happened to match. Accidental safety stops being
// safety the moment someone adds a catch-all route.
func validateClientOperation(operation cervorules.Operation) error {
	if operation == "" {
		return nil
	}
	return policyvocab.Vocabulary().ValidateOperation(operation)
}

func mergeRequestFacts(base, override requestFacts) requestFacts {
	out := base
	if override.ID != "" {
		out.ID = override.ID
	}
	if override.User != "" {
		out.User = override.User
	}
	if override.Channel != "" {
		out.Channel = override.Channel
	}
	if override.Risk != "" {
		out.Risk = override.Risk
	}
	// The caller's OperationHint is a FALLBACK: it fills in only when the
	// request itself didn't determine an operation (e.g. a profile-in-path
	// route like /v1/coding/chat/completions that matches no path rule). An
	// explicit X-Cervo-Capability header on the request must still win, so it
	// is not overridden here.
	if override.OperationHint != "" && out.OperationHint == "" {
		out.OperationHint = override.OperationHint
	}
	if override.Stream {
		out.Stream = true
	}
	if len(override.Metadata) > 0 {
		if out.Metadata == nil {
			out.Metadata = map[string]string{}
		}
		for key, value := range override.Metadata {
			out.Metadata[key] = value
		}
	}
	return out
}

func requestFromFacts(facts requestFacts) cervorules.Request {
	metadata := map[string]string{}
	for key, value := range facts.Metadata {
		metadata[key] = value
	}
	if facts.Risk != "" {
		metadata["risk"] = facts.Risk
	}
	if len(metadata) == 0 {
		metadata = nil
	}
	return cervorules.Request{
		ID:        facts.ID,
		User:      facts.User,
		Channel:   facts.Channel,
		Operation: facts.OperationHint,
		Metadata:  metadata,
	}
}
