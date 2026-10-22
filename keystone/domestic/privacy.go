package domestic

import (
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	"net/http"
)

// Each HTTP attempt, including redirects, rechecks the originating turn's
// current disclosure policy. Clone the client so concurrent turns never share it.
func privacyHTTPClient(client *http.Client) *http.Client {
	copy := *client
	transport := copy.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	copy.Transport = privacyTransport{transport}
	return &copy
}

type privacyTransport struct{ next http.RoundTripper }

func (t privacyTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if err := runtimedb.CheckPrivacyDelivery(request.Context(), runtimedb.MemoryDestination{}); err != nil {
		return nil, err
	}
	return t.next.RoundTrip(request)
}
