package embed

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/scrypster/muninndb/internal/plugin"
)

// providerHTTPError classifies a non-200 HTTP response from an OpenAI-compatible
// embed endpoint into a *plugin.ProviderError so the retroactive processor
// (internal/plugin/retroactive.go) can tell "this engram's content is
// permanently unembeddable" (400/404/413/422) apart from "the provider itself
// is down or overloaded" (429/5xx). The latter must never latch
// DigestEmbedFailed onto engrams that would embed fine once the provider
// recovers — that was exactly the bug behind the wholesale-batch blacklisting
// this type exists to prevent.
func providerHTTPError(provider string, resp *http.Response) error {
	// Drain a bounded amount so the transport can reuse the connection, but
	// never retain or log the provider's response body.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return &plugin.ProviderError{
		Provider:   provider,
		StatusCode: resp.StatusCode,
		Retryable:  retryableEmbedStatus(resp.StatusCode),
	}
}

// providerTransportError wraps a transport-level failure (timeout, connection
// refused, DNS failure). context.Canceled/DeadlineExceeded are left unwrapped
// (via %w) so callers can detect them directly with errors.Is — a caller
// cancellation or client-side deadline is not evidence about the provider
// either way. Everything else becomes a retryable ProviderError: a dead
// socket means the endpoint is unreachable, never that a specific text is bad.
func providerTransportError(provider string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s embed request: %w", provider, err)
	}
	return &plugin.ProviderError{Provider: provider, Retryable: true, Err: err}
}

// retryableEmbedStatus mirrors internal/plugin/enrich's classification of
// which HTTP statuses indicate a transient, provider-side condition rather
// than a permanent problem with this specific request's content.
func retryableEmbedStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}
