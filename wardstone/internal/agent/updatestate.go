package agent

// Self-update health gate, confirmation, and auto-revert (ADR-W003 W5,
// completing §A6). The whole gate is agent-side: no PromoteUpdate/
// RevertUpdate ops, no staged_only field — Keystone only ever sees
// SelfUpdate/SelfUpdateResult. The state file this package manages,
// update-state.json in Config.DataDir, is the only coordination surface
// between the promoting process and the (re-exec'd or systemd-restarted)
// process that must confirm or be reverted:
//
//   - selfUpdate (selfupdate.go) writes it right after staging the new
//     binary and right before promoting: {"pending": version, "deadline":
//     now+2m}.
//   - The FIRST successful Keystone session establishment after that
//     (confirmUpdateIfPending, called from session()) deletes it and logs
//     Info "self-update confirmed" — proof the promoted binary can talk to
//     Keystone.
//   - CheckUpdateState, called from wardstoned's main BEFORE agent.New/Run,
//     is the other half: if the file is still there and its deadline has
//     passed, the promoted binary evidently never confirmed (crashed
//     repeatedly, or never got far enough to dial out) — swap `-prev` back
//     over BinaryPath, delete the state file, log Error "self-update
//     reverted", and re-exec into the restored binary so the caller's
//     main() never runs the broken promoted image any further.
//
// The outer watchdog (packaging/wardstoned.service, Restart=always) is what
// gives a crashing promoted binary enough restart attempts to reach
// CheckUpdateState with an expired deadline in the first place.

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// updateDeadlineWindow is how long a newly-promoted binary has to establish
// a Keystone session before CheckUpdateState considers it dead and reverts.
const updateDeadlineWindow = 2 * time.Minute

// updateStateFileName is the name of the state file within DataDir. Kept as
// a named const (rather than inlined) since both the agent package and
// operators reading a data dir need the exact name.
const updateStateFileName = "update-state.json"

type updateState struct {
	Pending  string    `json:"pending"`
	Deadline time.Time `json:"deadline"`
}

func updateStatePath(dataDir string) string {
	return filepath.Join(dataDir, updateStateFileName)
}

func (a *Agent) updateStatePath() string {
	return updateStatePath(a.cfg.DataDir)
}

// writeUpdateState atomically writes the pending-update marker. Called by
// selfUpdate after staging the new binary and before promoting it.
func writeUpdateState(dataDir, version string) error {
	st := updateState{Pending: version, Deadline: time.Now().Add(updateDeadlineWindow)}
	b, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("marshal update state: %w", err)
	}
	if err := writeFileAtomic(updateStatePath(dataDir), b, 0o600); err != nil {
		return fmt.Errorf("write update state: %w", err)
	}
	return nil
}

// readUpdateState loads and parses the state file. ok is false if the file
// does not exist; err is non-nil only for a read/parse failure on a file
// that does exist (the caller should then remove it rather than get stuck).
func readUpdateState(dataDir string) (st updateState, ok bool, err error) {
	b, rerr := os.ReadFile(updateStatePath(dataDir))
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return updateState{}, false, nil
		}
		return updateState{}, false, rerr
	}
	if jerr := json.Unmarshal(b, &st); jerr != nil {
		return updateState{}, true, jerr
	}
	return st, true, nil
}

// confirmUpdateIfPending is the confirmation half of the health gate: if a
// self-update state file is present, this session establishment is proof
// the promoted binary works, so the file is removed and the confirmation is
// logged. A no-op (silently) when no state file exists, which is the common
// case on every ordinary session.
func (a *Agent) confirmUpdateIfPending() {
	path := a.updateStatePath()
	if _, err := os.Stat(path); err != nil {
		return
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		a.log.Warn("could not remove self-update state file after successful session", "error", err)
		return
	}
	a.log.Info("self-update confirmed")
}

// CheckUpdateState is the startup half of the self-update health gate
// (ADR-W003 W5). Call it from wardstoned's main, using the same DataDir and
// BinaryPath the agent will run with, BEFORE agent.New/Run:
//
//   - update-state.json absent: no-op, returns nil immediately.
//   - present, deadline not yet passed: left alone — Agent.Run's session
//     loop will confirm (delete it) on its first successful session, or a
//     later process start will find the deadline has passed and revert.
//   - present, deadline passed: BinaryPath-prev is renamed over BinaryPath,
//     the state file is deleted, an Error is logged, and — because the
//     calling process is itself either the broken promoted binary or a
//     fresh systemd-restarted instance of it — execSelf is invoked to
//     re-exec into the just-restored (known-good) binary so main() never
//     proceeds to run the broken image any further. On non-linux, execSelf
//     only logs (see execself_other.go); the caller's main must handle that
//     platform's restart itself.
//
// execSelf defaults to the real re-exec (cfg.ExecSelf nil); tests override
// cfg.ExecSelf to observe the revert without actually re-execing the test
// binary.
func CheckUpdateState(cfg Config, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	st, ok, err := readUpdateState(cfg.DataDir)
	if err != nil {
		log.Warn("corrupt self-update state file, removing", "error", err)
		os.Remove(updateStatePath(cfg.DataDir))
		return nil
	}
	if !ok || time.Now().Before(st.Deadline) {
		return nil
	}

	binaryPath := cfg.BinaryPath
	if binaryPath == "" {
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("resolve own binary: %w", err)
		}
		binaryPath = exe
	}
	prev := binaryPath + "-prev"
	if err := os.Rename(prev, binaryPath); err != nil {
		log.Error("self-update revert failed: could not restore previous binary",
			"error", err, "prev", prev, "pending_version", st.Pending)
		return err
	}
	if err := os.Remove(updateStatePath(cfg.DataDir)); err != nil && !os.IsNotExist(err) {
		log.Warn("self-update reverted but could not remove state file", "error", err)
	}
	log.Error("self-update reverted", "pending_version", st.Pending, "deadline", st.Deadline)

	execSelf := cfg.ExecSelf
	if execSelf == nil {
		execSelf = func() { realExecSelf(log, binaryPath) }
	}
	execSelf()
	return nil
}
