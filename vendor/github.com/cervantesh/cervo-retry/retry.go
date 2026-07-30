package cervoretry

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

type Classification struct {
	StatusCode      int
	Message         string
	BreakerEligible bool
	Retryable       bool
}

type BackoffPolicy struct {
	Initial    time.Duration
	Max        time.Duration
	Multiplier float64
}

func DefaultBackoff() BackoffPolicy {
	return BackoffPolicy{Initial: 250 * time.Millisecond, Max: 5 * time.Second, Multiplier: 2}
}

func (p BackoffPolicy) Delay(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	initial := p.Initial
	if initial <= 0 {
		initial = 250 * time.Millisecond
	}
	max := p.Max
	if max <= 0 {
		max = 5 * time.Second
	}
	multiplier := p.Multiplier
	if multiplier < 1 {
		multiplier = 1
	}
	delay := float64(initial)
	for i := 0; i < attempt; i++ {
		delay *= multiplier
		if time.Duration(delay) >= max {
			return max
		}
	}
	if time.Duration(delay) > max {
		return max
	}
	return time.Duration(delay)
}

func ClassifyTransportError(err error) Classification {
	message := fmt.Sprintf("transport error: %v", err)
	c := Classification{StatusCode: http.StatusBadGateway, Message: message, BreakerEligible: true, Retryable: true}
	if err == nil {
		return Classification{StatusCode: http.StatusOK, Message: "", BreakerEligible: false, Retryable: false}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		c.StatusCode = http.StatusGatewayTimeout
		c.Message = fmt.Sprintf("upstream timeout: %v", err)
		return c
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		c.StatusCode = http.StatusGatewayTimeout
		c.Message = fmt.Sprintf("upstream timeout: %v", err)
	}
	return c
}

func ClassifyHTTPStatus(statusCode int, responseBody string) Classification {
	message := strings.TrimSpace(responseBody)
	if message == "" {
		message = http.StatusText(statusCode)
	}
	c := Classification{StatusCode: statusCode, Message: fmt.Sprintf("HTTP %d: %s", statusCode, message)}
	switch {
	case statusCode == http.StatusTooManyRequests:
		c.BreakerEligible = true
		c.Retryable = true
		c.StatusCode = http.StatusServiceUnavailable
		c.Message = "upstream rate limited request (HTTP 429)"
	case statusCode >= 500:
		c.BreakerEligible = true
		c.Retryable = true
		c.StatusCode = http.StatusBadGateway
	case statusCode == http.StatusRequestTimeout:
		c.BreakerEligible = true
		c.Retryable = true
		c.StatusCode = http.StatusGatewayTimeout
	}
	return c
}

func IsRetryableStatus(statusCode int) bool {
	return statusCode == http.StatusTooManyRequests || statusCode == http.StatusRequestTimeout || statusCode >= 500
}
