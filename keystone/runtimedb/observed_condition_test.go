package runtimedb

import "testing"

func TestObservedConditionKeepsObservedProblemsVisibleWhileApprovalWaits(t *testing.T) {
	tests := []struct {
		name string
		row  Observed
		want OperatorCondition
	}{
		{"healthy", Observed{Healthy: true}, OperatorHealthy},
		{"healthy requested change", Observed{AwaitingApproval: true, Detail: "running: Docker container is healthy; managed configuration differs from desired state; Update is waiting for your approval"}, OperatorWaiting},
		{"exited awaiting repair", Observed{AwaitingApproval: true, Detail: "exited: Docker container stopped (exit code 2); Update is waiting for your approval"}, OperatorProblem},
		{"exited retained starting health", Observed{AwaitingApproval: true, AppHealth: "starting", Detail: "exited: Docker container stopped (exit code 2); Update is waiting for your approval"}, OperatorProblem},
		{"missing awaiting repair", Observed{AwaitingApproval: true, Detail: "missing: Docker container was not found; Create is waiting for your approval"}, OperatorProblem},
		{"configuration drift awaiting approval", Observed{AwaitingApproval: true, Detail: "drifted: managed Docker configuration differs from desired state; Update is waiting for your approval"}, OperatorWaiting},
		{"unhealthy converged", Observed{Healthy: true, AppHealth: "unhealthy"}, OperatorProblem},
		{"starting", Observed{Healthy: true, AppHealth: "starting"}, OperatorStarting},
		{"failed", Observed{Healthy: false}, OperatorProblem},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.row.Condition(); got != tt.want {
				t.Fatalf("condition = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestUpsertObservedDropsUnknownApplicationHealth(t *testing.T) {
	db := open(t)
	if err := db.UpsertObserved(Observed{Kind: "Container", Name: "app", Healthy: true, AppHealth: "private-token-value"}); err != nil {
		t.Fatal(err)
	}
	rows, err := db.ListObserved()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].AppHealth != "" {
		t.Fatalf("unknown application health reached durable status: %+v", rows)
	}
}
