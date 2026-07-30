// Package cervoretry provides shared retry helpers for HTTP and transport
// failures.
//
// The package exposes small, dependency-free building blocks:
// classification of upstream HTTP statuses, classification of transport
// errors, and exponential backoff delay calculation. Callers can use the
// returned Classification values to decide whether an operation should be
// retried and whether the failure should count toward a circuit breaker.
package cervoretry
