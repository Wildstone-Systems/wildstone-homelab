package leader

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/kubeapi"
)

// Prober is one server's API, as the leader's identity.
type Prober interface {
	Server() string
	DryRunCreateConfigMap(ctx context.Context, cm *kubeapi.ConfigMap) error
}

// Barrier is the epoch barrier (safety contract #180 §2.6). The fence
// reads the authority record through each API server's own cache, which
// can lag or, after its binding is re-created, freeze. So before a term
// writes, the new leader sends a dry-run write stamped with its epoch to
// every server's local API server. Only a server whose fence holds this
// leader and this epoch admits it.
type Barrier struct {
	// Servers is every server node's local API server. All must admit.
	Servers []Prober
	// Timeout is how long the barrier may stay shut before it is an
	// incident (default 30s). Interval is the pause between probes
	// (default 200ms).
	Timeout  time.Duration
	Interval time.Duration
}

// The probe: a ConfigMap in the fenced namespace, never stored.
const (
	ProbeNamespace    = "wildstone-apps"
	probeGenerateName = "wildstone-epoch-barrier-"
	epochAnnotation   = "wildstone.dev/epoch"
)

// StuckError is a barrier that never opened: the named servers' fences
// don't hold the new term. The fix is restarting k3s on them; nothing
// writes until then.
type StuckError struct {
	Epoch   uint64
	Servers []string
	Last    map[string]error
}

func (e *StuckError) Error() string {
	parts := make([]string, 0, len(e.Servers))
	for _, s := range e.Servers {
		parts = append(parts, fmt.Sprintf("%s (%v)", s, e.Last[s]))
	}
	return fmt.Sprintf("leader: the epoch %d barrier stayed shut on %s; restart k3s there", e.Epoch, strings.Join(parts, ", "))
}

// Wait returns nil once every server has admitted a probe in epoch.
func (b Barrier) Wait(ctx context.Context, epoch uint64) error {
	if len(b.Servers) == 0 {
		return errors.New("leader: the barrier needs at least one server")
	}
	if epoch == 0 {
		return errors.New("leader: epoch 0 has no leader; there is nothing to open")
	}
	timeout, interval := b.Timeout, b.Interval
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	if interval == 0 {
		interval = 200 * time.Millisecond
	}
	deadline := time.Now().Add(timeout)
	pending := map[string]Prober{}
	for _, p := range b.Servers {
		pending[p.Server()] = p
	}
	last := map[string]error{}
	for {
		for name, p := range pending {
			cm := &kubeapi.ConfigMap{Metadata: kubeapi.ObjectMeta{
				Namespace:    ProbeNamespace,
				GenerateName: probeGenerateName,
				Annotations:  map[string]string{epochAnnotation: strconv.FormatUint(epoch, 10)},
			}}
			pctx, cancel := context.WithTimeout(ctx, interval*5)
			err := p.DryRunCreateConfigMap(pctx, cm)
			cancel()
			if err == nil {
				delete(pending, name)
				continue
			}
			last[name] = err
		}
		if len(pending) == 0 {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			stuck := &StuckError{Epoch: epoch, Last: map[string]error{}}
			for name := range pending {
				stuck.Servers = append(stuck.Servers, name)
				stuck.Last[name] = last[name]
			}
			sort.Strings(stuck.Servers)
			return stuck
		}
		if !sleep(ctx, interval) {
			return ctx.Err()
		}
	}
}
