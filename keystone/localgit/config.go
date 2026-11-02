// Package localgit renders bootstrap configuration for established OpenSSH/Git
// serving. It implements no listener, Git protocol, account or key management.
package localgit

import (
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config describes an independently installed source for one deployment repo.
// It contains references and network restrictions only, never key material.
type Config struct {
	ListenAddress    string `json:"listenAddress"`
	Port             int    `json:"port"`
	ClientCIDR       string `json:"clientCIDR"`
	ServiceUser      string `json:"serviceUser"`
	Repository       string `json:"repository"`
	MirrorRepository string `json:"mirrorRepository"`
	InstallDirectory string `json:"installDirectory"`
	Namespace        string `json:"namespace"`
	SourceName       string `json:"sourceName"`
	CredentialSecret string `json:"credentialSecret"`
}

var safePath = regexp.MustCompile(`^/[a-zA-Z0-9_./-]+$`)
var safeUser = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,30}$`)
var kubeName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)
var tailnet = func() *net.IPNet { _, n, _ := net.ParseCIDR("100.64.0.0/10"); return n }()

func private(ip net.IP) bool {
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || tailnet.Contains(ip))
}
func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && (rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".."))
}

func (c Config) Validate() error {
	if !private(net.ParseIP(c.ListenAddress)) {
		return fmt.Errorf("local source: a specific private or loopback listen IP is required")
	}
	if c.Port < 1024 || c.Port > 65535 {
		return fmt.Errorf("local source: use an unprivileged dedicated port")
	}
	ip, n, err := net.ParseCIDR(c.ClientCIDR)
	if err != nil || !private(ip) || !ip.Equal(n.IP) {
		return fmt.Errorf("local source: explicit private client CIDR required")
	}
	// A private-looking start address must not authorize a wider public range.
	last := append(net.IP(nil), n.IP...)
	for i := range last {
		last[i] |= ^n.Mask[i]
	}
	if !private(last) {
		return fmt.Errorf("local source: client CIDR includes public addresses")
	}
	ones, bits := n.Mask.Size()
	if ones == 0 || (tailnet.Contains(ip) && bits == 32 && ones < 10) {
		return fmt.Errorf("local source: client CIDR is too broad")
	}
	if !safeUser.MatchString(c.ServiceUser) || c.ServiceUser == "root" || c.ServiceUser == "nobody" {
		return fmt.Errorf("local source: dedicated unprivileged service user required")
	}
	for _, p := range []string{c.Repository, c.MirrorRepository, c.InstallDirectory} {
		if !safePath.MatchString(p) || filepath.Clean(p) != p || p == "/" || strings.Contains(p, "/../") {
			return fmt.Errorf("local source: clean absolute installation paths without whitespace required")
		}
	}
	if within(c.Repository, c.MirrorRepository) || within(c.MirrorRepository, c.Repository) || within(c.Repository, c.InstallDirectory) || within(c.InstallDirectory, c.Repository) || within(c.MirrorRepository, c.InstallDirectory) || within(c.InstallDirectory, c.MirrorRepository) {
		return fmt.Errorf("local source: serving keys/configuration and deployment repository must be separate")
	}
	for _, s := range []string{c.Namespace, c.SourceName, c.CredentialSecret} {
		if !kubeName.MatchString(s) {
			return fmt.Errorf("local source: invalid Kubernetes reference")
		}
	}
	if c.Namespace == "default" || c.Namespace == "flux-system" || strings.HasPrefix(c.Namespace, "kube-") || strings.HasPrefix(c.Namespace, "wildstone-") {
		return fmt.Errorf("local source: use the isolated application namespace")
	}
	return nil
}

// Render returns non-secret bootstrap files. Install is a separately reviewed
// host action; no account, key, daemon, firewall or Kubernetes change occurs.
func Render(c Config) (map[string][]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	// This is an exact command allowlist, never eval of SSH_ORIGINAL_COMMAND.
	// No AcceptEnv is configured, and env -i prevents inherited Git redirection.
	wrapper := fmt.Sprintf(`#!/bin/sh
set -eu
if [ "${SSH_ORIGINAL_COMMAND-}" != "git-upload-pack '/deployment.git'" ]; then
  echo 'Only read-only deployment Git retrieval is permitted.' >&2
  exit 126
fi
if [ ! -f '%s/%s' ] || [ -L '%s' ]; then
  echo 'A dedicated source mirror is required.' >&2
  exit 126
fi
exec /usr/bin/env -i PATH=/usr/bin:/bin GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_NO_REPLACE_OBJECTS=1 \
  /usr/bin/git -c core.hooksPath=/dev/null \
  -c transfer.hideRefs=HEAD -c transfer.hideRefs=refs/ -c 'transfer.hideRefs=!refs/heads/deploy' \
  -c uploadpack.allowTipSHA1InWant=false -c uploadpack.allowReachableSHA1InWant=false \
  -c uploadpack.allowAnySHA1InWant=false \
  upload-pack --strict '%s'
`, c.MirrorRepository, mirrorMarker, c.MirrorRepository, c.MirrorRepository)
	config := fmt.Sprintf(`# Dedicated local source only. No Include of the host SSH configuration.
Port %d
ListenAddress %s
HostKey %s/ssh_host_ed25519_key
PidFile %s/sshd.pid
AuthorizedKeysFile %s/authorized_keys
AllowUsers %s@%s
AuthenticationMethods publickey
PubkeyAuthentication yes
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin no
UsePAM yes
StrictModes yes
ForceCommand /bin/sh %s/upload-pack
DisableForwarding yes
PermitTTY no
PermitUserRC no
PermitUserEnvironment no
MaxAuthTries 3
MaxSessions 1
MaxStartups 4:50:8
LoginGraceTime 15
ClientAliveInterval 30
ClientAliveCountMax 2
LogLevel VERBOSE
`, c.Port, c.ListenAddress, c.InstallDirectory, c.InstallDirectory, c.InstallDirectory, c.ServiceUser, c.ClientCIDR, c.InstallDirectory)
	unit := fmt.Sprintf(`[Unit]
Description=Wildstone read-only local Git source
After=network.target

[Service]
Type=simple
ExecStart=/usr/sbin/sshd -D -e -f %s/sshd_config
Restart=on-failure
RestartSec=5s
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=%s
ReadOnlyPaths=%s
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX

[Install]
WantedBy=multi-user.target
`, c.InstallDirectory, c.InstallDirectory, c.MirrorRepository)
	u := url.URL{Scheme: "ssh", User: url.User(c.ServiceUser), Host: net.JoinHostPort(c.ListenAddress, fmt.Sprint(c.Port)), Path: "/deployment.git"}
	source := map[string]any{"apiVersion": "source.toolkit.fluxcd.io/v1", "kind": "GitRepository", "metadata": map[string]any{"name": c.SourceName, "namespace": c.Namespace}, "spec": map[string]any{"interval": "1m", "timeout": "30s", "url": u.String(), "ref": map[string]any{"branch": "deploy"}, "secretRef": map[string]any{"name": c.CredentialSecret}, "recurseSubmodules": false}}
	sourceYAML, err := yaml.Marshal(source)
	if err != nil {
		return nil, err
	}
	return map[string][]byte{"upload-pack": []byte(wrapper), "sshd_config": []byte(config), "wildstone-git-source.service": []byte(unit), "gitrepository.yaml": sourceYAML, "init-mirror": []byte(initMirror(c)), "mirror": []byte(mirror(c)), "wildstone-git-mirror.service": []byte(mirrorUnit(c)), "wildstone-git-mirror.timer": []byte(mirrorTimer)}, nil
}

const mirrorMarker = "wildstone-source-mirror-v1"

// The serving object database is separate from private prepared candidates.
// Git hideRefs is not an object-confidentiality boundary (gitnamespaces SECURITY).
func initMirror(c Config) string {
	return fmt.Sprintf(`#!/bin/sh
set -eu
umask 077
if [ -e '%s' ]; then
  echo 'Refusing to initialize or adopt an existing mirror.' >&2
  exit 126
fi
/bin/mkdir '%s'
/usr/bin/env -i PATH=/usr/bin:/bin GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null \
  /usr/bin/git init --bare --object-format=sha1 --initial-branch=deploy '%s'
printf '1\n' > '%s/%s'
`, c.MirrorRepository, c.MirrorRepository, c.MirrorRepository, c.MirrorRepository, mirrorMarker)
}

func mirror(c Config) string {
	return fmt.Sprintf(`#!/bin/sh
set -eu
if [ ! -f '%s/%s' ] || [ -L '%s' ]; then
  echo 'A dedicated initialized source mirror is required.' >&2
  exit 126
fi
# file:// forces normal upload-pack transfer, never clone --local hardlinks or
# shared alternates. Only the approved ref is requested; prepared refs are not.
exec /usr/bin/env -i PATH=/usr/bin:/bin GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_NO_REPLACE_OBJECTS=1 \
  /usr/bin/git --git-dir='%s' -c core.hooksPath=/dev/null \
  -c protocol.allow=never -c protocol.file.allow=always -c protocol.ext.allow=never \
  fetch --atomic --no-tags --no-write-fetch-head --no-recurse-submodules \
  'file://%s' 'refs/heads/deploy:refs/heads/deploy'
`, c.MirrorRepository, mirrorMarker, c.MirrorRepository, c.MirrorRepository, c.Repository)
}

func mirrorUnit(c Config) string {
	return fmt.Sprintf(`[Unit]
Description=Copy the promoted local ref to the read-only Git source
After=network.target

[Service]
Type=oneshot
User=%s
ExecStart=/bin/sh %s/mirror
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=read-only
ReadOnlyPaths=%s
ReadWritePaths=%s
PrivateDevices=yes
PrivateNetwork=yes
RestrictAddressFamilies=AF_UNIX
`, c.ServiceUser, c.InstallDirectory, c.Repository, c.MirrorRepository)
}

const mirrorTimer = `[Unit]
Description=Refresh the local read-only Git source

[Timer]
OnBootSec=15s
OnUnitInactiveSec=15s
Unit=wildstone-git-mirror.service

[Install]
WantedBy=timers.target
`
