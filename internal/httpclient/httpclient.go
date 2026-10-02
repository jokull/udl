// Package httpclient holds the defaults every outbound request in udl carries.
//
// The rule exists because it was learned the hard way: a host that rejects Go's
// default "Go-http-client/1.1" (NZBFinder answers it with a Cloudflare 403) will
// reject whichever request forgets to set a User-Agent, and the failure looks
// like a bad release rather than a bad request. One place to set it means one
// place to get it right.
package httpclient

import "net/http"

// UserAgent identifies udl to the services it talks to.
const UserAgent = "udl/1.0 (+https://github.com/jokull/udl)"

// Set applies the shared defaults to req.
func Set(req *http.Request) {
	req.Header.Set("User-Agent", UserAgent)
}
