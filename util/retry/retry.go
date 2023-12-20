package retry

import (
	"net"
	"net/url"
	"strings"

	taskererrs "github.com/kubeTasker/kubeTasker/errors"
	apierr "k8s.io/apimachinery/pkg/api/errors"
)

// IsRetryableKubeAPIError returns if the error is a retryable kubernetes error
func IsRetryableKubeAPIError(err error) bool {
	err = taskererrs.Cause(err)
	if apierr.IsNotFound(err) || apierr.IsForbidden(err) || apierr.IsInvalid(err) || apierr.IsMethodNotSupported(err) {
		return false
	}
	return true
}

// IsRetryableNetworkError returns whether or not the error is a retryable network error
func IsRetryableNetworkError(err error) bool {
	if err == nil {
		return false
	}
	err = taskererrs.Cause(err)
	errStr := err.Error()

	switch err.(type) {
	case net.Error:
		switch err.(type) {
		case *net.DNSError, *net.OpError, net.UnknownNetworkError:
			return true
		case *url.Error:
			if strings.Contains(errStr, "Connection closed by foreign host") {
				return true
			}
		default:
			if strings.Contains(errStr, "net/http: TLS handshake timeout") {
				return true
			} else if strings.Contains(errStr, "i/o timeout") {
				return true
			} else if strings.Contains(errStr, "connection timed out") {
				return true
			}
		}
	}
	return false
}
