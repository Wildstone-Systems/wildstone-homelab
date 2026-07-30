package agent

// fstab management for EnsureMount (ADR-W003 W6c). Pure string/file
// manipulation, kept portable (no build tag) even though EnsureMount's
// mounting half is Linux-only, since the fstab format itself is not
// platform-specific and this makes the byte-identical-outside-the-block
// invariant easy to unit test on any host.

import (
	"fmt"
	"os"
	"strings"
)

const (
	fstabBeginMarker = "# BEGIN wildstone-managed"
	fstabEndMarker   = "# END wildstone-managed"
)

// fstabLine renders one fstab entry. dump/pass are always "0 0" — Wardstone
// does not manage fsck ordering.
func fstabLine(source, target, fstype string, options []string) string {
	opts := strings.Join(options, ",")
	if opts == "" {
		opts = "defaults"
	}
	return fmt.Sprintf("%s\t%s\t%s\t%s\t0\t0", source, target, fstype, opts)
}

// ensureFstabEntry idempotently ensures that exactly one line for `target`
// exists inside the agent-owned marker block ("# BEGIN wildstone-managed" /
// "# END wildstone-managed") of fstabPath, replacing any prior line for
// that same target. It NEVER reads or rewrites anything outside that
// block — content above and below the markers (or the whole file, if no
// block exists yet) is preserved byte-for-byte apart from the block's own
// insertion/update. The write is atomic (tmp+rename, matching
// writeManagedFile's pattern). Returns updated=false when the resulting
// file content is unchanged from what's already on disk (i.e. an
// identical entry already existed) — the caller uses this to report
// fstab_updated accurately and skip an unnecessary write.
func ensureFstabEntry(fstabPath, target, line string) (updated bool, err error) {
	origBytes, err := os.ReadFile(fstabPath)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	orig := string(origBytes)

	var lines []string
	trailingNewline := false
	if orig != "" {
		trailingNewline = strings.HasSuffix(orig, "\n")
		lines = strings.Split(orig, "\n")
		if trailingNewline {
			lines = lines[:len(lines)-1]
		}
	}

	beginIdx, endIdx := -1, -1
	for i, l := range lines {
		switch {
		case beginIdx == -1 && strings.TrimSpace(l) == fstabBeginMarker:
			beginIdx = i
		case beginIdx != -1 && endIdx == -1 && strings.TrimSpace(l) == fstabEndMarker:
			endIdx = i
		}
	}

	var before, blockLines, after []string
	if beginIdx != -1 && endIdx != -1 && endIdx > beginIdx {
		before = lines[:beginIdx]
		blockLines = append([]string{}, lines[beginIdx+1:endIdx]...)
		after = lines[endIdx+1:]
	} else {
		before = lines
	}

	var newBlockLines []string
	found := false
	for _, l := range blockLines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		fields := strings.Fields(l)
		if len(fields) >= 2 && fields[1] == target {
			found = true
			newBlockLines = append(newBlockLines, line)
			continue
		}
		newBlockLines = append(newBlockLines, l)
	}
	if !found {
		newBlockLines = append(newBlockLines, line)
	}

	var out []string
	out = append(out, before...)
	out = append(out, fstabBeginMarker)
	out = append(out, newBlockLines...)
	out = append(out, fstabEndMarker)
	out = append(out, after...)

	newContent := strings.Join(out, "\n")
	if newContent != "" {
		newContent += "\n"
	}

	if newContent == orig {
		return false, nil
	}
	if err := writeFileAtomic(fstabPath, []byte(newContent), 0o644); err != nil {
		return false, err
	}
	return true, nil
}
