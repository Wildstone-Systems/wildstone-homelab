package butlerlive

import (
	"errors"
	"fmt"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butler"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	"strings"
)

// CompleteProposalTask runs after the API's shared proposal pipeline persists
// every requested Plan. It cannot satisfy an objective to execute the change.
func (bs *Butlers) CompleteProposalTask(taskID string, planIDs []string, calls []butler.ToolCallRecord) (bool, error) {
	return bs.completeProposalTask(taskID, planIDs, calls, true)
}

// PreserveProposalTask saves proof without trusting a classification from a failed turn.
func (bs *Butlers) PreserveProposalTask(taskID string, planIDs []string, calls []butler.ToolCallRecord) error {
	_, err := bs.completeProposalTask(taskID, planIDs, calls, false)
	return err
}
func (bs *Butlers) completeProposalTask(taskID string, planIDs []string, calls []butler.ToolCallRecord, complete bool) (bool, error) {

	if taskID == "" || len(planIDs) == 0 {
		return false, nil
	}
	task, err := bs.DB.GetButlerTask(taskID)
	if err != nil {
		return false, err
	}
	if bs.Ledger == nil {
		return false, fmt.Errorf("no completion ledger")
	}
	if err = bs.Ledger.Fence(); err != nil {
		return false, err
	}
	if err = bs.Ledger.Authorize(taskID); err != nil {
		return false, err
	}
	label, err := bs.DB.TaskButler(taskID)
	if err != nil {
		return false, err
	}
	if label == "" {
		label = task.AgentClass
	}
	var evidence []butler.LedgerEvidence
	var proposalSteps []int64
	for _, call := range calls {
		if call.ResourceYAML != "" && !call.Rejected && call.Error == "" {
			proposalSteps = append(proposalSteps, call.StepSeq)
		}
	}
	if len(proposalSteps) != len(planIDs) {
		return false, fmt.Errorf("proposal persistence cannot be linked to its operations")
	}
	steps, err := bs.DB.ListButlerTaskSteps(taskID)
	if err != nil {
		return false, err
	}
	valid := map[int64]bool{}
	for _, step := range steps {
		if step.Kind == runtimedb.ButlerStepTool && step.Name == "propose_change" && step.State == runtimedb.ButlerStepVerified {
			valid[step.Seq] = true
		}
	}
	for i, id := range planIDs {
		if !valid[proposalSteps[i]] {
			return false, fmt.Errorf("proposal has no verified originating operation")
		}

		record, err := bs.DB.GetPlan(id)
		if err != nil {
			return false, err
		}
		if record.Principal != "butler" || record.ActingFor != task.Owner {
			return false, fmt.Errorf("proposal does not belong to this task's owner")
		}
		receipt, err := bs.DB.RecordButlerEffectReceipt(taskID, runtimedb.EffectReceipt{Kind: "executor", SourceID: id, Step: proposalSteps[i], Outcome: "succeeded", ProposalOnly: true, PermissionLabel: label, Audience: task.Audience})
		if err != nil {
			return false, err
		}
		evidence = append(evidence, butler.LedgerEvidence{Kind: "external-verification", Description: "Plan persisted for Owner review; proposed changes have not been applied", SourceID: id, ReceiptID: receipt, PermissionLabel: label, Audience: string(task.Audience)})
	}
	// Proof survives a failed verifier; classification must succeed before it can complete.
	if !complete || task.ObjectiveKind != runtimedb.ObjectiveEffectful {
		return false, nil
	}
	if err = bs.Ledger.Running(taskID, "proposal persistence confirmed"); err != nil {
		return false, err
	}
	summary := "Created the requested proposals for Owner review. The proposed changes have not been applied."
	if !task.ProposalOnly {
		descriptions, e := bs.DB.ButlerRequiredEffectDescriptions(taskID)
		if e != nil {
			return false, e
		}
		summary = "Completed: " + strings.Join(descriptions, "; ") + ". The proposed changes have not been applied."
	}
	if err = bs.Ledger.Complete(taskID, evidence, summary); err != nil {
		if errors.Is(err, runtimedb.ErrEffectReceiptRequired) {
			return false, bs.Ledger.Wait(taskID, butler.LedgerWaitReply, "one or more required effects still need confirmation")
		}
		return false, err
	}
	return true, nil
}
