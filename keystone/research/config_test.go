package research

import "testing"

func TestProviderConfigDefaultsAndRefusals(t *testing.T) {
	c, err := (Config{Name: "Public research", Kind: DDGS}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Backends) != 1 || c.Backends[0] != "duckduckgo" || len(c.FallbackOrder) != 0 || c.Limits != DefaultLimits() {
		t.Fatalf("defaults: %+v", c)
	}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.Kind = SearXNG }, func(c *Config) { c.Kind = Tavily }, func(c *Config) { c.Backends = []string{"auto"} },
		func(c *Config) { c.Backends = []string{} }, func(c *Config) { c.Backends = []string{"duckduckgo", "google"} },
		func(c *Config) { c.FallbackOrder = []string{"duckduckgo"} }, func(c *Config) { c.EndpointRef = "https://example.com" },
		func(c *Config) { c.EndpointRef = "service/../ddgs" }, func(c *Config) { c.CredentialRef = "REPLACED-credential" },
		func(c *Config) { c.Limits.RequestTimeoutSeconds = 31 }, func(c *Config) { c.Limits.SearchesPerTask = -1 },
		func(c *Config) { c.Limits.DownloadBytes = 6 * 1024 * 1024 }, func(c *Config) { c.Name = "bad\nname" },
	} {
		bad := c
		mutate(&bad)
		if _, err := bad.Normalize(); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	c.EndpointRef = "service/ddgs"
	c.CredentialRef = "secret/ddgs"
	if _, err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
}
