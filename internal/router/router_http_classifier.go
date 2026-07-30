package router

import (
	"net/http"

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
		ID:            httpFacts.ID,
		User:          httpFacts.User,
		Channel:       httpFacts.Channel,
		Risk:          httpFacts.Risk,
		OperationHint: httpFacts.Operation,
		Metadata:      map[string]string{},
	}
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
	if override.OperationHint != "" {
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
