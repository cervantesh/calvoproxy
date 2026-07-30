// Package proxy defines shared CervoSoft proxy request and response contracts.
//
// The DTOs intentionally mirror the small OpenAI-compatible chat and embedding
// shapes exchanged between proxy clients, the proxy service, and upstream
// callers.
//
// Extraction boundary: contracts packages should stay DTO-first and must not
// import runtime clients, databases, or service packages.
package proxy
