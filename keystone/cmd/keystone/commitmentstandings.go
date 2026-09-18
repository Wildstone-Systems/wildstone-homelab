package main

// The adapter between the commitment pass and the agent's read tool.
//
// It lives here because keystone/butler is the model and keystone/upkeep
// is the deterministic pass, and neither should import the other: an
// engine that depended on the model would be one an outage could stop
// working out what is due, and a model package that depended on the
// pass's timer would carry it into every test. cmd/keystone already
// holds both, so the conversion belongs at the place that wires them.
//
// It also does the reduction: a model is handed the verdict and the
// reason, never the anchors, positions and lead thresholds the
// occurrence carries. The arithmetic is the one part already settled and
// provable, and handing it to something that will do arithmetic with it
// is inviting a second, worse answer.

import (
	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/upkeep"
)

type commitmentStandings struct{ engine *upkeep.Engine }

func (c commitmentStandings) Describe() ([]butler.CommitmentLine, error) {
	standings, err := c.engine.Evaluate()
	if err != nil {
		return nil, err
	}
	var out []butler.CommitmentLine
	for _, st := range standings {
		if st.Disabled {
			continue
		}
		state := string(st.Occurrence.State)
		// "can't tell" rather than the pending it technically is. A
		// model told a stale meter is pending will reassure somebody,
		// which is the one thing A11 exists to prevent.
		if len(st.Occurrence.Unresolved) > 0 && st.Occurrence.Trigger == "" {
			state = "can't tell"
		}
		waits := ""
		if len(st.MayComplete) > 0 {
			waits = st.MayComplete[0]
		}
		out = append(out, butler.CommitmentLine{
			Name: st.Name, State: state, Reason: st.Occurrence.Reason, WaitsOn: waits,
		})
	}
	return out, nil
}
