//go:build linux

package agent

// EnsureMount is, per ADR-W003, "the most dangerous op in the product": it
// performs a real syscall.Mount and edits /etc/fstab. Every check here is a
// hard agent-side invariant enforced regardless of what Keystone asked for
// (ADR-W003 §A4) — do not weaken any of them without an ADR amendment.
//
// Ordering guarantee: the mount is attempted FIRST; /etc/fstab is only
// touched after a successful (or already-satisfied) mount. A mount that
// fails must never leave a boot-time landmine entry in fstab that would
// fail the machine's next boot.

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

// mountFn is a seam over syscall.Mount so tests can record calls without
// ever mounting anything for real. Real-mount coverage belongs to the VM
// harness in testing/e2e, not this unit-level suite.
var mountFn = syscall.Mount

// mountinfoPath is /proc/self/mountinfo, used to detect an already-mounted
// target so EnsureMount is idempotent. Package-level var so tests can point
// it at a fixture file.
var mountinfoPath = "/proc/self/mountinfo"

// allowedFstypes is the closed set EnsureMount will mount. Anything else is
// FORBIDDEN — this is a security boundary, not a UX nicety (e.g. no
// "bind" mounts, which could be used to re-expose an unmanaged host path).
var allowedFstypes = map[string]bool{
	"nfs": true, "nfs4": true, "cifs": true, "ext4": true, "xfs": true,
}

// mountOptionRe bounds every mount option to a safe character set: no
// commas (fstab field separator — a comma inside an option string could
// inject a second, attacker-chosen option) and no whitespace.
var mountOptionRe = regexp.MustCompile(`^[a-zA-Z0-9._=/-]+$`)

// requiredMountOptions are always present in the final option set,
// appended if the caller omitted them. nosuid/nodev are the minimum bar
// for a homelab-managed remote/managed mount: no setuid binaries, no
// device nodes, executable off of it.
var requiredMountOptions = []string{"nosuid", "nodev"}

func (a *Agent) ensureMount(op *wsv1.EnsureMount) (*wsv1.OpResult_EnsureMount, *wsv1.OpError) {
	target, oerr := a.resolveManaged(op.GetTarget())
	if oerr != nil {
		return nil, oerr
	}
	// Second hard boundary on top of the managed-prefix check: EnsureMount
	// additionally requires the target to resolve under Config.MountRoot
	// (prod default /srv/wildstone). Tests configure MountRoot to a temp
	// dir that is ALSO one of ManagedPrefixes, so both checks are real.
	if target != a.cfg.MountRoot && !strings.HasPrefix(target, a.cfg.MountRoot+string(filepath.Separator)) {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_FORBIDDEN,
			"mount target %q must resolve under %q", op.GetTarget(), a.cfg.MountRoot)
	}
	if !allowedFstypes[op.GetFstype()] {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_FORBIDDEN,
			"fstype %q not allowed (nfs, nfs4, cifs, ext4, xfs only)", op.GetFstype())
	}
	if op.GetSource() == "" {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_INVALID, "source is required")
	}
	// The source and resolved target are written verbatim into an /etc/fstab
	// line whose fields are whitespace-separated. Any whitespace (a newline
	// especially) would inject an attacker-chosen fstab entry — a boot-time
	// landmine, exactly what the mount-before-fstab ordering guarantee exists
	// to prevent. Options are already charset-bounded below; hold the source
	// and target to the same standard.
	if hasFstabHostileRune(op.GetSource()) {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_FORBIDDEN,
			"mount source %q contains whitespace or control characters (fstab injection risk)", op.GetSource())
	}
	if hasFstabHostileRune(target) {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_FORBIDDEN,
			"mount target %q contains whitespace or control characters (fstab injection risk)", op.GetTarget())
	}
	for _, o := range op.GetOptions() {
		if !mountOptionRe.MatchString(o) {
			return nil, opErr(wsv1.ErrorCode_ERROR_CODE_FORBIDDEN,
				"mount option %q contains disallowed characters (comma/whitespace injection risk)", o)
		}
	}
	options := ensureRequiredOptions(append([]string{}, op.GetOptions()...), requiredMountOptions...)

	alreadyMounted, err := isMounted(mountinfoPath, target)
	if err != nil {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "check mountinfo: %v", err)
	}

	mounted := false
	if !alreadyMounted {
		if err := os.MkdirAll(target, 0o755); err != nil {
			return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "mkdir target: %v", err)
		}
		// syscall.Mount takes flag-type options (nosuid, nodev, ro, ...) as
		// MS_* bits, NOT in the data string — a kernel fs driver handed
		// "nosuid" in data rejects the mount (EINVAL) or silently ignores
		// it, so passing them as data would defeat the very options this
		// handler force-appends. Split them out; only fs-specific options
		// travel in data. The fstab line keeps the full word list (fstab
		// wants words, mount(8) does this same translation).
		flags, data := mountFlagsAndData(options)
		if err := mountFn(op.GetSource(), target, op.GetFstype(), flags, data); err != nil {
			// Mount failed: stop here. fstab must NOT be touched — see the
			// ordering guarantee in the file comment.
			return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "mount: %v", err)
		}
		mounted = true
	}

	line := fstabLine(op.GetSource(), target, op.GetFstype(), options)
	fstabUpdated, err := ensureFstabEntry(a.cfg.FstabPath, target, line)
	if err != nil {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "fstab update: %v", err)
	}

	return &wsv1.OpResult_EnsureMount{EnsureMount: &wsv1.EnsureMountResult{
		Mounted:      mounted,
		FstabUpdated: fstabUpdated,
	}}, nil
}

// flagOptions maps fstab option words that are mount(2) flags, not
// fs-specific data. Words not listed here pass through in the data string.
var flagOptions = map[string]uintptr{
	"nosuid":  syscall.MS_NOSUID,
	"nodev":   syscall.MS_NODEV,
	"noexec":  syscall.MS_NOEXEC,
	"ro":      syscall.MS_RDONLY,
	"noatime": syscall.MS_NOATIME,
}

func mountFlagsAndData(options []string) (uintptr, string) {
	var flags uintptr
	var data []string
	for _, o := range options {
		if bit, ok := flagOptions[o]; ok {
			flags |= bit
			continue
		}
		data = append(data, o)
	}
	return flags, strings.Join(data, ",")
}

func ensureRequiredOptions(options []string, required ...string) []string {
	have := map[string]bool{}
	for _, o := range options {
		have[o] = true
	}
	for _, r := range required {
		if !have[r] {
			options = append(options, r)
		}
	}
	return options
}

// isMounted reports whether target already appears as a mount point in the
// mountinfo file at path (field 5, per the kernel's
// Documentation/filesystems/proc.rst format). A missing mountinfo file is
// treated as "not mounted" rather than an error, matching /proc's
// existence guarantees in a real (non-test) environment.
func isMounted(path, target string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		if fields[4] == target {
			return true, nil
		}
	}
	return false, sc.Err()
}
