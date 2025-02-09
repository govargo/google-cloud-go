// Copyright 2023 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package httptransport

import (
	"context"
	"net"
	"net/http"
	"time"

	"cloud.google.com/go/auth"
	"cloud.google.com/go/auth/detect"
	"cloud.google.com/go/auth/internal"
	"go.opencensus.io/plugin/ochttp"
	"golang.org/x/net/http2"
)

const (
	quotaProjectHeaderKey = "X-Goog-User-Project"
)

func newTransport(base http.RoundTripper, opts *Options) (http.RoundTripper, error) {
	var headers = opts.Headers
	ht := &headerTransport{
		base:    base,
		headers: headers,
	}
	var trans http.RoundTripper = ht
	trans = addOCTransport(trans, opts)
	switch {
	case opts.DisableAuthentication:
		// Do nothing.
	case opts.APIKey != "":
		qp := internal.GetQuotaProject(nil, opts.Headers.Get(quotaProjectHeaderKey))
		if qp != "" {
			if headers == nil {
				headers = make(map[string][]string, 1)
			}
			headers.Set(quotaProjectHeaderKey, qp)
		}
		trans = &apiKeyTransport{
			Transport: trans,
			Key:       opts.APIKey,
		}
	default:
		creds, err := detect.DefaultCredentials(opts.resolveDetectOptions())
		if err != nil {
			return nil, err
		}
		qp := creds.QuotaProjectID()
		if qp != "" {
			if headers == nil {
				headers = make(map[string][]string, 1)
			}
			headers.Set(quotaProjectHeaderKey, qp)
		}

		var tp auth.TokenProvider = creds
		if opts.TokenProvider != nil {
			tp = opts.TokenProvider
		}
		trans = &authTransport{
			base:     trans,
			provider: auth.NewCachedTokenProvider(tp, nil),
		}
	}
	return trans, nil
}

// defaultBaseTransport returns the base HTTP transport.
// On App Engine, this is urlfetch.Transport.
// Otherwise, use a default transport, taking most defaults from
// http.DefaultTransport.
// If TLSCertificate is available, set TLSClientConfig as well.
func defaultBaseTransport(dialTLSContext func(context.Context, string, string) (net.Conn, error)) http.RoundTripper {
	trans := http.DefaultTransport.(*http.Transport).Clone()
	trans.MaxIdleConnsPerHost = 100

	// TODO(codyoss): readd in a future PR
	// if clientCertSource != nil {
	// 	trans.TLSClientConfig = &tls.Config{
	// 		GetClientCertificate: clientCertSource,
	// 	}
	// }
	if dialTLSContext != nil {
		// If DialTLSContext is set, TLSClientConfig wil be ignored
		trans.DialTLSContext = dialTLSContext
	}

	// Configures the ReadIdleTimeout HTTP/2 option for the
	// transport. This allows broken idle connections to be pruned more quickly,
	// preventing the client from attempting to re-use connections that will no
	// longer work.
	http2Trans, err := http2.ConfigureTransports(trans)
	if err == nil {
		http2Trans.ReadIdleTimeout = time.Second * 31
	}

	return trans
}

type apiKeyTransport struct {
	// Key is the API Key to set on requests.
	Key string
	// Transport is the underlying HTTP transport.
	// If nil, http.DefaultTransport is used.
	Transport http.RoundTripper
}

func (t *apiKeyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	newReq := *req
	args := newReq.URL.Query()
	args.Set("key", t.Key)
	newReq.URL.RawQuery = args.Encode()
	return t.Transport.RoundTrip(&newReq)
}

type headerTransport struct {
	headers http.Header
	base    http.RoundTripper
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt := t.base
	newReq := *req
	newReq.Header = make(http.Header)
	for k, vv := range req.Header {
		newReq.Header[k] = vv
	}

	for k, v := range t.headers {
		newReq.Header[k] = v
	}

	return rt.RoundTrip(&newReq)
}

func addOCTransport(trans http.RoundTripper, opts *Options) http.RoundTripper {
	if opts.DisableTelemetry {
		return trans
	}
	return &ochttp.Transport{
		Base:        trans,
		Propagation: &httpFormat{},
	}
}

type authTransport struct {
	provider auth.TokenProvider
	base     http.RoundTripper
}

// RoundTrip authorizes and authenticates the request with an
// access token from Transport's Source. Per the RoundTripper contract we must
// not modify the initial request, so we clone it, and we must close the body
// on any errors that happens during our token logic.
func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	reqBodyClosed := false
	if req.Body != nil {
		defer func() {
			if !reqBodyClosed {
				req.Body.Close()
			}
		}()
	}
	token, err := t.provider.Token(req.Context())
	if err != nil {
		return nil, err
	}
	req2 := req.Clone(req.Context())
	SetAuthHeader(token, req2)
	reqBodyClosed = true
	return t.base.RoundTrip(req2)
}
