// Package requestmeta defines standard HTTP metadata headers and helpers for
// tenant and bearer-token propagation.
//
// Extraction boundary: requestmeta may depend on configenv until both packages
// are extracted together or tenant lookup is injected.
package requestmeta
