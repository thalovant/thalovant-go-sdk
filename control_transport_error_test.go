package thalovant

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

type failingControlRoundTripper struct{ err error }

func (t failingControlRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, t.err
}
func TestControlTransportErrorsDoNotExposeQueryOrCustomCause(t *testing.T) {
	querySecret := "query-" + NewRequestID()
	causeSecret := "cause-" + NewRequestID()
	customCause := errors.New("synthetic transport diagnostics " + causeSecret)
	for _, cause := range []error{customCause, &url.Error{Op: "Post", URL: "https://example.invalid/?credential=" + querySecret, Err: customCause}} {
		control := NewControlPlane("https://example.invalid/?credential="+querySecret, "synthetic-bearer")
		control.HTTPClient = &http.Client{Transport: failingControlRoundTripper{err: cause}}
		_, err := control.GetHub(context.Background(), "synthetic")
		if !errors.Is(err, ErrAPI) {
			t.Fatal("missing API error category")
		}
		if strings.Contains(err.Error(), querySecret) || strings.Contains(err.Error(), causeSecret) || errors.Is(err, customCause) {
			t.Fatal("credential-bearing transport diagnostics escaped sanitization")
		}
		var transportCause *url.Error
		if errors.As(err, &transportCause) {
			t.Fatal("unsafe URL cause retained")
		}
	}
	control := NewControlPlane("https://example.invalid/%zz?credential="+querySecret, "synthetic-bearer")
	_, err := control.GetHub(context.Background(), "synthetic")
	if !errors.Is(err, ErrAPI) || strings.Contains(err.Error(), querySecret) {
		t.Fatal("invalid URL escaped sanitized API error")
	}
}
func TestControlTransportErrorsKeepOnlyStandardContextCause(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		var ctx context.Context
		var cancel context.CancelFunc
		if deadline {
			ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		} else {
			ctx, cancel = context.WithCancel(context.Background())
			cancel()
		}
		control := NewControlPlane("https://example.invalid/?credential="+NewRequestID(), "synthetic-bearer")
		control.HTTPClient = &http.Client{Transport: failingControlRoundTripper{err: errors.New("unsafe custom diagnostic")}}
		_, err := control.GetHub(ctx, "synthetic")
		cancel()
		if !errors.Is(err, ErrAPI) || !errors.Is(err, ctx.Err()) || strings.Contains(err.Error(), "unsafe custom") {
			t.Fatal("context classification lost or custom cause retained")
		}
	}
}
