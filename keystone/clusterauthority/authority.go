// Package clusterauthority is Keystone's half of the leader fence (safety
// contract #180 §2.1). When a Wardstone wins the leader Lease it asks
// Keystone for an epoch; Keystone records it in its runtime database and
// then in the cluster's authority record, which the fencing admission
// policy reads. Keystone's Kubernetes identity can update that one
// ConfigMap and nothing else.
//
// Nothing calls this yet: the Wardstone-to-Keystone request is wired in
// slice 3c.
package clusterauthority

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/kubeapi"
)

// Where the authority record lives (deploy/ansible/templates/authority.yaml.j2).
const (
	Namespace = "wildstone-system"
	Name      = "wildstone-authority"
)

// runtimeIdentity is a Wardstone runtime account's API username
// (deploy/ansible/templates/runtime-rbac.yaml.j2).
var runtimeIdentity = regexp.MustCompile(`^system:serviceaccount:wildstone-system:wardstone-[a-z0-9]([-a-z0-9.]{0,51}[a-z0-9])?$`)

// Store issues epochs; runtimedb.DB is the implementation.
type Store interface {
	IssueLeaderEpoch(leader string, floor uint64) (uint64, error)
}

// Records reads and compare-and-swaps the authority record.
type Records interface {
	GetConfigMap(ctx context.Context, ns, name string) (*kubeapi.ConfigMap, error)
	UpdateConfigMap(ctx context.Context, cm *kubeapi.ConfigMap) (*kubeapi.ConfigMap, error)
}

// Leases reads the leader Lease.
type Leases interface {
	GetLease(ctx context.Context, ns, name string) (*kubeapi.Lease, error)
}

// Where the leader Lease lives (wardstone/internal/leader).
const (
	LeaseNamespace = "wildstone-system"
	LeaseName      = "wardstone-leader"
)

// Authority issues leader epochs.
type Authority struct {
	Store   Store
	Records Records
	Leases  Leases
}

// ErrNotHolder means the asker doesn't hold the leader Lease, so it gets
// no epoch: a node that lost an election, or one asking for another node.
var ErrNotHolder = errors.New("clusterauthority: the asker does not hold the leader Lease")

// ErrBadRecord means the authority record is missing or unreadable. It is
// an incident: the fence refuses every write until it is repaired, and
// Keystone never recreates it with a guessed epoch.
var ErrBadRecord = errors.New("clusterauthority: the authority record is missing or unreadable")

// maxAttempts bounds compare-and-swap retries when the record moves.
const maxAttempts = 5

// Issue makes leader the holder of a new epoch and returns it. The epoch
// is in Keystone's database before it is in the cluster, and the record is
// only ever moved forward, by compare-and-swap on its resourceVersion.
func (a Authority) Issue(ctx context.Context, leader string) (uint64, error) {
	if !runtimeIdentity.MatchString(leader) {
		return 0, fmt.Errorf("clusterauthority: %q is not a Wardstone runtime identity", leader)
	}
	for attempt := 0; attempt < maxAttempts; attempt++ {
		cm, err := a.Records.GetConfigMap(ctx, Namespace, Name)
		if err != nil {
			if kubeapi.IsNotFound(err) {
				return 0, ErrBadRecord
			}
			return 0, fmt.Errorf("clusterauthority: reading the authority record: %w", err)
		}
		current, err := strconv.ParseUint(cm.Data["epoch"], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%w: epoch %q", ErrBadRecord, cm.Data["epoch"])
		}
		// The Lease decides who may ask; an epoch for anyone else would
		// fence out the node that won. It is checked on every attempt,
		// after the record is read: if the write below then succeeds, the
		// record didn't move since this node was seen holding the Lease,
		// and a node that wins later asks later and writes over it.
		if err := a.holds(ctx, leader); err != nil {
			return 0, err
		}
		epoch, err := a.Store.IssueLeaderEpoch(leader, current)
		if err != nil {
			return 0, err
		}
		cm.Data = map[string]string{"epoch": strconv.FormatUint(epoch, 10), "leader": leader}
		if _, err := a.Records.UpdateConfigMap(ctx, cm); err != nil {
			if kubeapi.IsConflict(err) {
				// Someone wrote the record since it was read. The epoch
				// just issued is burnt, never reused; read again.
				continue
			}
			return 0, fmt.Errorf("clusterauthority: writing epoch %d: %w", epoch, err)
		}
		return epoch, nil
	}
	return 0, fmt.Errorf("clusterauthority: the authority record kept changing; gave up after %d attempts", maxAttempts)
}

// holds checks that leader holds the leader Lease now.
func (a Authority) holds(ctx context.Context, leader string) error {
	l, err := a.Leases.GetLease(ctx, LeaseNamespace, LeaseName)
	if err != nil {
		if kubeapi.IsNotFound(err) {
			return ErrNotHolder
		}
		return fmt.Errorf("clusterauthority: reading the leader Lease: %w", err)
	}
	if l.Spec.HolderIdentity != leader {
		return fmt.Errorf("%w: it is held by %q", ErrNotHolder, l.Spec.HolderIdentity)
	}
	return nil
}
