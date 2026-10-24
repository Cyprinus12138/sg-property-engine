// Package mcpserver exposes the engine's query API as Model Context
// Protocol tools. It is built on the public Go client, so it serves the
// same data whether it runs inside `sgpe serve` (at /mcp, calling the
// handlers in-process) or as `sgpe mcp` over stdio against a running engine.
//
// Every tool is read-only. Tool inputs take the smallest handle that
// identifies a thing (a project name, a district number) and the server
// resolves and normalises it; place anchors are resolved to coordinates
// here so an unknown name is an error rather than an unfiltered result.
package mcpserver

import (
	"context"
	"net/http"
	"net/http/httptest"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Cyprinus12138/sg-property-engine/client"
)

const instructions = `Singapore residential property data from official sources: HDB resale
transactions (data.gov.sg), URA private-property transactions, rentals,
pipeline and developer sales, MRT/LRT exits and other POIs, and the CEA
salesperson register.

Conventions:
- Prices and rents are SGD. psf is SGD per square foot; areas are square metres.
- Dates are YYYY-MM or YYYY-MM-DD. HDB and URA data are monthly; URA lags by weeks.
- Districts are postal districts 1-28. market_segment is CCR, RCR or OCR.
- Names: use search_properties first when you only have a partial or informal
  name, then pass the exact name it returns.
- "near" centres a search on a point, project, POI or HDB block; results carry
  distance_m and the resolved anchor. "has_poi" keeps rows with a POI nearby.
- An empty result means no matching rows in the database, which may also mean
  that source has not been synced yet: check data_freshness before saying
  something does not exist.`

// New builds an MCP server whose tools call the engine through c.
func New(c *client.Client, version string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{
		Name:    "sg-property-engine",
		Title:   "Singapore property data",
		Version: version,
	}, &mcp.ServerOptions{Instructions: instructions})
	addTools(s, &tools{c: c})
	return s
}

// InProcessClient returns a client that serves every request by calling h
// directly, with no network hop. Caching is off: the database is local and
// results should reflect the latest ingest.
func InProcessClient(h http.Handler) *client.Client {
	return client.New("http://sgpe.internal").
		WithHTTPClient(&http.Client{Transport: handlerTransport{h}}).
		WithCacheTTL(0)
}

type handlerTransport struct{ h http.Handler }

func (t handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	t.h.ServeHTTP(rec, r)
	return rec.Result(), nil
}

// HTTPHandler serves s over the streamable HTTP transport. Stateless: the
// tools need no session, so nothing is held between requests. Browser
// cross-origin requests are refused; the SDK separately refuses
// non-localhost Host headers on loopback connections (DNS rebinding).
func HTTPHandler(s *mcp.Server) http.Handler {
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s },
		&mcp.StreamableHTTPOptions{Stateless: true})
	return http.NewCrossOriginProtection().Handler(h)
}

// RunStdio serves s on stdin/stdout until the client disconnects or ctx ends.
func RunStdio(ctx context.Context, s *mcp.Server) error {
	return s.Run(ctx, &mcp.StdioTransport{})
}
