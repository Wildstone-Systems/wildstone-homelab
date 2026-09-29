package recovery

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/proto/recoveryseal"
)

type Kit struct {
	Format                  string            `json:"format"`
	Endpoint                string            `json:"endpoint"`
	Bucket                  string            `json:"bucket"`
	Prefix                  string            `json:"prefix,omitempty"`
	Region                  string            `json:"region,omitempty"`
	PathStyle               bool              `json:"pathStyle,omitempty"`
	ReadOnlyAccessKeyID     string            `json:"readOnlyAccessKeyId"`
	ReadOnlySecretAccessKey string            `json:"readOnlySecretAccessKey"`
	RepositoryPasswords     map[string]string `json:"repositoryPasswords"`
	RecoveryPrivateKey      string            `json:"recoveryPrivateKey"`
}

func LoadKit(path string) (Kit, error) {
	var kit Kit
	body, err := os.ReadFile(path)
	if err != nil {
		return kit, fmt.Errorf("recovery: reading kit: %w", err)
	}
	defer clear(body)
	if err := json.Unmarshal(body, &kit); err != nil {
		return Kit{}, fmt.Errorf("recovery: decoding kit: %w", err)
	}
	if kit.Format != "wildstone-recovery-kit-v1" || kit.Endpoint == "" || kit.Bucket == "" ||
		kit.ReadOnlyAccessKeyID == "" || kit.ReadOnlySecretAccessKey == "" ||
		kit.RepositoryPasswords[PlanName] == "" {
		return Kit{}, fmt.Errorf("recovery: kit is incomplete")
	}
	u, err := url.Parse(kit.Endpoint)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return Kit{}, fmt.Errorf("recovery: kit endpoint is invalid")
	}
	if err := recoveryseal.ValidatePrivateKey(kit.RecoveryPrivateKey); err != nil {
		return Kit{}, fmt.Errorf("recovery: kit private key is invalid")
	}
	return kit, nil
}

func (k Kit) Repository(plan string) string {
	parts := []string{strings.TrimSuffix(k.Endpoint, "/"), k.Bucket}
	if k.Prefix != "" {
		parts = append(parts, strings.Trim(k.Prefix, "/"))
	}
	return "s3:" + strings.Join(append(parts, plan), "/")
}
