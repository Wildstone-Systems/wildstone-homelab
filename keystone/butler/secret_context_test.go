package butler

import (
	"context"
	"encoding/json"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler/provider"
	"strings"
	"testing"
)

func TestModelNeverReceivesARawValue(t *testing.T) {
	raw := "generated-low-entropy-value"
	for _, input := range []string{"my password is " + raw, "/secret personal WiFi = " + raw, "/secret household WiFi = " + raw} {
		t.Run(input[:10], func(t *testing.T) {
			sp := &scriptedProvider{responses: []*provider.ChatResponse{{Text: "Use the protected entry form."}}}
			sess := &Session{Gateway: newTestToolGateway(newTestStore(t), nil), History: []provider.Message{{Role: "user", Text: input}}}
			if _, err := sess.Run(context.Background(), sp, "continue"); err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(sp.requests)
			if strings.Contains(string(b), raw) {
				t.Fatal("continuation sent raw credential to provider")
			}
		})
	}
}
