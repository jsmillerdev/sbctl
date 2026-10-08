package awsapi

import (
	"context"
	"time"
)

// SetRetryHooks replaces how the client waits between tries and the random number behind the
// jitter, for the service calls and the metadata service alike. A nil argument is left as it is.
func SetRetryHooks(c *Client, sleep func(context.Context, time.Duration) error, jitter func() float64) {
	for _, r := range []*retrier{c.core.retry, c.IMDS.retry} {
		if sleep != nil {
			r.sleep = sleep
		}
		if jitter != nil {
			r.jitter = jitter
		}
	}
}
