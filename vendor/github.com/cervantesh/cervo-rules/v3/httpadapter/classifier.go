package httpadapter

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/cervantesh/cervo-rules/v3/core"
)

// HeaderOptions lists alternate HTTP headers for optional classification.
type HeaderOptions struct {
	RequestID []string
	User      []string
	Channel   []string
	Operation []string
	Risk      []string
}

// PathOperation maps an HTTP method/path shape to a policy operation.
type PathOperation struct {
	Methods   []string
	Prefix    string
	Contains  string
	Regex     string
	Operation core.Operation
}

// HTTPClassificationOptions configures the optional HTTP adapter.
type HTTPClassificationOptions struct {
	Headers           HeaderOptions
	PathOperations    []PathOperation
	ChannelOperations map[string]core.Operation
	CopyHeaders       bool
}

// RequestFacts is a transport-derived view that can be converted to core.Request.
type RequestFacts struct {
	ID        string
	User      string
	Channel   string
	Risk      string
	Operation core.Operation
	Headers   http.Header
}

// Request converts facts into a core request and stores risk as metadata.
func (f RequestFacts) Request() core.Request {
	metadata := map[string]string{}
	if f.Risk != "" {
		metadata["risk"] = f.Risk
	}
	if len(metadata) == 0 {
		metadata = nil
	}
	return core.Request{
		ID:        f.ID,
		User:      f.User,
		Channel:   f.Channel,
		Operation: f.Operation,
		Metadata:  metadata,
	}
}

// Classifier is immutable after construction and safe for concurrent use.
type Classifier struct {
	options HTTPClassificationOptions
	paths   []compiledPathOperation
}

type compiledPathOperation struct {
	methods   map[string]struct{}
	prefix    string
	contains  string
	regex     *regexp.Regexp
	operation core.Operation
}

// NewClassifier precompiles path rules for production use.
func NewClassifier(options HTTPClassificationOptions) (*Classifier, error) {
	classifier := &Classifier{options: options}
	for _, rule := range options.PathOperations {
		compiled := compiledPathOperation{
			prefix:    strings.ToLower(rule.Prefix),
			contains:  strings.ToLower(rule.Contains),
			operation: core.NewOperation(string(rule.Operation)),
		}
		if len(rule.Methods) > 0 {
			compiled.methods = map[string]struct{}{}
			for _, method := range rule.Methods {
				compiled.methods[strings.ToUpper(strings.TrimSpace(method))] = struct{}{}
			}
		}
		if strings.TrimSpace(rule.Regex) != "" {
			re, err := regexp.Compile(rule.Regex)
			if err != nil {
				return nil, fmt.Errorf("compile path operation regex %q: %w", rule.Regex, err)
			}
			compiled.regex = re
		}
		classifier.paths = append(classifier.paths, compiled)
	}
	if classifier.options.ChannelOperations != nil {
		normalized := map[string]core.Operation{}
		for channel, operation := range classifier.options.ChannelOperations {
			normalized[strings.ToLower(strings.TrimSpace(channel))] = core.NewOperation(string(operation))
		}
		classifier.options.ChannelOperations = normalized
	}
	return classifier, nil
}

// FactsFromHTTPRequest classifies r without mutating classifier state.
func (c *Classifier) FactsFromHTTPRequest(r *http.Request) RequestFacts {
	if c == nil || r == nil {
		return RequestFacts{}
	}
	facts := RequestFacts{
		ID:      firstHeader(r, c.options.Headers.RequestID),
		User:    firstHeader(r, c.options.Headers.User),
		Channel: firstHeader(r, c.options.Headers.Channel),
		Risk:    firstHeader(r, c.options.Headers.Risk),
	}
	if c.options.CopyHeaders {
		facts.Headers = r.Header.Clone()
	}
	if operation := firstHeader(r, c.options.Headers.Operation); operation != "" {
		facts.Operation = core.NewOperation(operation)
	}
	if facts.Operation == "" {
		facts.Operation = c.inferPathOperation(r)
	}
	if facts.Operation == "" && facts.Channel != "" {
		facts.Operation = c.options.ChannelOperations[strings.ToLower(strings.TrimSpace(facts.Channel))]
	}
	return facts
}

func (c *Classifier) inferPathOperation(r *http.Request) core.Operation {
	path := strings.ToLower(r.URL.Path)
	method := strings.ToUpper(r.Method)
	for _, rule := range c.paths {
		if len(rule.methods) > 0 {
			if _, ok := rule.methods[method]; !ok {
				continue
			}
		}
		if rule.prefix != "" && !strings.HasPrefix(path, rule.prefix) {
			continue
		}
		if rule.contains != "" && !strings.Contains(path, rule.contains) {
			continue
		}
		if rule.regex != nil && !rule.regex.MatchString(path) {
			continue
		}
		return rule.operation
	}
	return ""
}

func firstHeader(r *http.Request, headers []string) string {
	for _, header := range headers {
		if value := strings.TrimSpace(r.Header.Get(header)); value != "" {
			return value
		}
	}
	return ""
}
