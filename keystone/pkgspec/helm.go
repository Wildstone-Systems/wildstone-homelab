package pkgspec

import (
	"net/url"
	"path"
	"regexp"
	"strings"
)

// HelmSpec describes reviewed upstream bytes, not a command or workload.
// It is intentionally unavailable to the ordinary Package install path.
type HelmSpec struct {
	SourceURL    string `yaml:"sourceURL" json:"sourceURL"`
	SourceCommit string `yaml:"sourceCommit" json:"sourceCommit"`
	ChartPath    string `yaml:"chartPath" json:"chartPath"`
	ChartVersion string `yaml:"chartVersion" json:"chartVersion"`
	Image        string `yaml:"image" json:"image"`
}

var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var chartVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
var pinnedImagePattern = regexp.MustCompile(`^[a-z0-9./_-]+:[a-zA-Z0-9._-]+@sha256:[0-9a-f]{64}$`)

func validateHelm(p *Package) error {
	h := p.Helm
	if h == nil {
		return nil
	}
	// First slice supports only the reviewed Audiobookshelf values mapping.
	// A new application needs a reviewed mapping, not arbitrary values input.
	if p.Metadata.Name != "audiobooks-audiobookshelf" || p.Metadata.Capability != "audiobooks" || p.Metadata.Profile != "audiobookshelf" {
		return &ValidationError{Field: "helm", Msg: "only the experimental Audiobookshelf profile is supported"}
	}
	if len(p.Templates) != 0 || len(p.Lifecycle.PreInstall) != 0 || len(p.Lifecycle.PostInstall) != 0 {
		return &ValidationError{Field: "helm", Msg: "cannot mix chart projection with resource templates or hooks"}
	}
	u, err := url.Parse(h.SourceURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return &ValidationError{Field: "helm.sourceURL", Msg: "must be an HTTPS source without credentials, query or fragment"}
	}
	if !commitPattern.MatchString(h.SourceCommit) {
		return &ValidationError{Field: "helm.sourceCommit", Msg: "must pin a full commit"}
	}
	if h.ChartPath == "" || path.Clean(h.ChartPath) != h.ChartPath || strings.HasPrefix(h.ChartPath, "/") || strings.Contains(h.ChartPath, "..") || strings.ContainsAny(h.ChartPath, "\\\n\r") {
		return &ValidationError{Field: "helm.chartPath", Msg: "must be a clean relative chart path"}
	}
	if !chartVersionPattern.MatchString(h.ChartVersion) {
		return &ValidationError{Field: "helm.chartVersion", Msg: "must pin a chart version"}
	}
	if !pinnedImagePattern.MatchString(h.Image) {
		return &ValidationError{Field: "helm.image", Msg: "must pin image tag and sha256 digest"}
	}
	return nil
}
