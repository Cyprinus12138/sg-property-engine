// Package sourcestest points the upstream clients in package sources at an
// httptest server, so client and ingest tests exercise the real request
// building and response parsing without touching the network.
//
//	srv := httptest.NewServer(mux)
//	dg := sources.NewDataGov()
//	dg.SetHTTPClient(sourcestest.Client(srv))
//
// Every request keeps its path and query but is sent to srv. The host it
// was meant for is passed in the X-Upstream-Host header, so one fake can
// serve several upstreams (data.gov.sg and its S3 download host, say).
package sourcestest

import (
	"net/http"
	"net/http/httptest"
	"net/url"
)

// UpstreamHostHeader carries the original request host to the fake server.
const UpstreamHostHeader = "X-Upstream-Host"

// Client returns an *http.Client that redirects every request to srv.
func Client(srv *httptest.Server) *http.Client {
	target, err := url.Parse(srv.URL)
	if err != nil {
		panic(err)
	}
	return &http.Client{Transport: &rewrite{target: target, next: srv.Client().Transport}}
}

type rewrite struct {
	target *url.URL
	next   http.RoundTripper
}

func (r *rewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	out.Header.Set(UpstreamHostHeader, req.URL.Host)
	out.URL.Scheme = r.target.Scheme
	out.URL.Host = r.target.Host
	out.Host = r.target.Host
	return r.next.RoundTrip(out)
}
