package agent

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// The Docker Engine client is reachable only through runtimeBackend: no
// production file but docker.go names its types (so none can make a raw
// Engine call), and only agent.go constructs it. A second backend then needs no
// caller to change (ADR-W050 §3 step 1).
func TestDockerStaysBehindTheRuntimeSeam(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	dockerOnly := map[string]bool{"dockerClient": true, "dockerError": true, "callWithHeaders": true}
	for _, f := range files {
		if f == "docker.go" || strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if n, ok := n.(*ast.Ident); ok {
				if dockerOnly[n.Name] {
					t.Errorf("%s uses %s outside the Docker backend", f, n.Name)
				}
				if n.Name == "newDockerClient" && f != "agent.go" {
					t.Errorf("%s constructs a Docker client; only agent.go wires the runtime", f)
				}
			}
			return true
		})
	}
}
