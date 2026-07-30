// Package cervomodelpolicy resolves model profiles, aliases, and ordered
// fallback chains for applications that route requests across language models.
//
// The package is intentionally limited to deterministic model selection. It
// does not authorize requests, call providers, retry provider failures, or
// enforce tenant policy. Applications should perform those decisions before or
// after calling Policy.Select.
package cervomodelpolicy
