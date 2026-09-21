package domestic

import "fmt"

// HouseholdExecutionAuthority is supplied by the runtime store, which checks
// this process's lease and the household dispatch stop on every call.
type HouseholdExecutionAuthority interface{ CheckHouseholdExecution() error }

func checkExecution(authority HouseholdExecutionAuthority) error {
	if authority == nil {
		return fmt.Errorf("household execution refused: no coordinator authority is wired")
	}
	return authority.CheckHouseholdExecution()
}
