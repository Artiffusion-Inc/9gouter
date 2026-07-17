package rtk

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// filter is an RTK text compressor. It may be nil when no filter applies.
type filter func(string) string

// autoDetectFilter mirrors open-sse/rtk/autodetect.js.
func autoDetectFilter(text string) filter {
	head := text
	if len(head) > DetectWindow {
		head = head[:DetectWindow]
	}

	if reGitLog.MatchString(head) {
		return gitLog
	}
	if reGitDiff.MatchString(head) || reGitDiffHunk.MatchString(head) {
		return gitDiffFilter
	}
	if reGitStatus.MatchString(head) {
		return gitStatus
	}
	if reBuildOutput.MatchString(head) {
		return buildOutput
	}
	if isMostlyPorcelain(head) {
		return gitStatus
	}

	lines := strings.Split(head, "\n")
	var nonEmpty []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			nonEmpty = append(nonEmpty, l)
		}
	}

	first5 := nonEmpty
	if len(first5) > 5 {
		first5 = first5[:5]
	}
	for _, l := range first5 {
		if isGrepLine(l) {
			return grep
		}
	}

	if len(nonEmpty) >= 3 && all(nonEmpty, isPathLike) {
		return find
	}

	if reTreeGlyph.MatchString(head) {
		return tree
	}

	if reLsTotal.MatchString(head) || countMatches(head, reLsRow) >= 3 {
		return ls
	}

	if reSearchListHeader.MatchString(head) {
		return searchList
	}

	if len(lines) >= SmartTruncateMinLines && isLineNumbered(lines) {
		return readNumbered
	}

	if len(nonEmpty) >= 5 && hasDuplicates(nonEmpty) {
		return dedupLog
	}

	if len(strings.Split(text, "\n")) >= SmartTruncateMinLines {
		return smartTruncate
	}

	return nil
}

func hasDuplicates(ss []string) bool {
	seen := map[string]bool{}
	for _, s := range ss {
		if seen[s] {
			return true
		}
		seen[s] = true
	}
	return false
}

var (
	reGitDiff       = regexp.MustCompile(`(?m)^diff --git `)
	reGitDiffHunk   = regexp.MustCompile(`(?m)^@@ `)
	reGitStatus     = regexp.MustCompile(`(?m)^On branch |^nothing to commit|^Changes (not |to be )|^Untracked files:`)
	reGitLog        = regexp.MustCompile(`(?m)^[|/\\ ]*commit [0-9a-f]{7,40}$`)
	rePorcelain     = regexp.MustCompile(`^[ MADRCU?!][ MADRCU?!] \S`)
	reBuildOutput   = regexp.MustCompile(`(?im)^(npm (warn|error|ERR!)|yarn (warn|error)|\s*Compiling\s+\S+|\s*Downloading\s+\S+|added \d+ package|\[ERROR\]|BUILD (SUCCESS|FAILED)|\s*Finished\s+|Successfully (installed|built)|ERROR:)`)
	reTreeGlyph     = regexp.MustCompile(`[├└]──|│  `)
	reLsRow         = regexp.MustCompile(`(?m)^[-dlbcps][rwx-]{9}`)
	reLsTotal       = regexp.MustCompile(`(?m)^total \d+$`)
	reReadNumbered  = regexp.MustCompile(`^\s*\d+\|`) // READ_NUMBERED_LINE_RE
	reSearchListHeader *regexp.Regexp
)

func init() {
	reSearchListHeader = regexp.MustCompile(`Result of search in '.*' \(total \d+ files\):`)
}

func isGrepLine(line string) bool {
	first := strings.Index(line, ":")
	if first < 0 {
		return false
	}
	second := strings.Index(line[first+1:], ":")
	if second < 0 {
		return false
	}
	lineno := line[first+1 : first+1+second]
	_, err := strconv.Atoi(lineno)
	return err == nil
}

func isPathLike(line string) bool {
	t := strings.TrimSpace(line)
	if t == "" {
		return false
	}
	if regexp.MustCompile(`^[A-Za-z]:[\\/]`).MatchString(t) {
		return true
	}
	if strings.Contains(t, ":") {
		return false
	}
	return strings.HasPrefix(t, ".") || strings.HasPrefix(t, "/") || strings.Contains(t, "/")
}

func isMostlyPorcelain(head string) bool {
	lines := strings.Split(head, "\n")
	var trimmed []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			trimmed = append(trimmed, l)
		}
	}
	if len(trimmed) < 3 {
		return false
	}
	hits := 0
	for _, l := range trimmed {
		if rePorcelain.MatchString(l) {
			hits++
		}
	}
	return float64(hits)/float64(len(trimmed)) >= 0.6
}

func isLineNumbered(lines []string) bool {
	hits, nonEmpty := 0, 0
	sample := lines
	if len(sample) > 100 {
		sample = sample[:100]
	}
	for _, l := range sample {
		if l == "" {
			continue
		}
		nonEmpty++
		if reReadNumbered.MatchString(l) {
			hits++
		}
	}
	if nonEmpty < 5 {
		return false
	}
	return float64(hits)/float64(nonEmpty) >= ReadNumberedMinHitRatio
}

func countMatches(text string, re *regexp.Regexp) int {
	cg := regexp.MustCompile(re.String())
	return len(cg.FindAllString(text, -1))
}

func all(ss []string, pred func(string) bool) bool {
	for _, s := range ss {
		if !pred(s) {
			return false
		}
	}
	return true
}

// gitDiff compacts a unified diff.
func gitDiff(input string, maxLines ...int) string {
	max := 500
	if len(maxLines) > 0 && maxLines[0] > 0 {
		max = maxLines[0]
	}
	return gitDiffInternal(input, max)
}

var gitDiffFilter filter = func(input string) string { return gitDiff(input) }

func gitDiffInternal(input string, maxLines int) string {
	result := []string{}
	currentFile := ""
	added, removed := 0, 0
	inHunk := false
	hunkShown := 0
	hunkSkipped := 0
	wasTruncated := false
	maxHunkLines := GitDiffHunkMaxLines

	lines := strings.Split(input, "\n")

	flushHunk := func() {
		if hunkSkipped > 0 {
			result = append(result, fmt.Sprintf("  ... (%d lines truncated)", hunkSkipped))
			wasTruncated = true
			hunkSkipped = 0
		}
	}

outer:
	for _, line := range lines {
		if strings.HasPrefix(line, "diff --git") {
			flushHunk()
			if currentFile != "" && (added > 0 || removed > 0) {
				result = append(result, fmt.Sprintf("  +%d -%d", added, removed))
			}
			parts := strings.Split(line, " b/")
			if len(parts) > 1 {
				currentFile = strings.Join(parts[1:], " b/")
			} else {
				currentFile = "unknown"
			}
			result = append(result, "", currentFile)
			added, removed = 0, 0
			inHunk = false
			hunkShown = 0
		} else if strings.HasPrefix(line, "@@") {
			flushHunk()
			inHunk = true
			hunkShown = 0
			result = append(result, fmt.Sprintf("  %s", line))
		} else if inHunk {
			if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
				added++
				if hunkShown < maxHunkLines {
					result = append(result, fmt.Sprintf("  %s", line))
					hunkShown++
				} else {
					hunkSkipped++
				}
			} else if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
				removed++
				if hunkShown < maxHunkLines {
					result = append(result, fmt.Sprintf("  %s", line))
					hunkShown++
				} else {
					hunkSkipped++
				}
			} else if hunkShown < maxHunkLines && !strings.HasPrefix(line, "\\") {
				if hunkShown > 0 {
					result = append(result, fmt.Sprintf("  %s", line))
					hunkShown++
				}
			}
		}

		if len(result) >= maxLines {
			result = append(result, "", "... (more changes truncated)")
			wasTruncated = true
			break outer
		}
	}

	flushHunk()
	if currentFile != "" && (added > 0 || removed > 0) {
		result = append(result, fmt.Sprintf("  +%d -%d", added, removed))
	}
	if wasTruncated {
		result = append(result, "[full diff: rtk git diff --no-compact]")
	}
	return strings.Join(result, "\n")
}

// gitStatus compacts git status output.
func gitStatus(input string) string {
	lines := strings.Split(input, "\n")
	if len(lines) == 0 || (len(lines) == 1 && strings.TrimSpace(lines[0]) == "") {
		return "Clean working tree"
	}

	branch := ""
	var stagedFiles, modifiedFiles, untrackedFiles []string
	staged, modified, untracked, conflicts := 0, 0, 0, 0

	reLongBranch := regexp.MustCompile(`^On branch (\S+)`)
	reLongMatch := regexp.MustCompile(`^\s*(modified|new file|deleted|renamed|both modified):\s+(.+)$`)

	for _, raw := range lines {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		if m := reLongBranch.FindStringSubmatch(raw); m != nil {
			branch = m[1]
			continue
		}
		if strings.HasPrefix(raw, "##") {
			branch = strings.TrimPrefix(raw, "## ")
			continue
		}
		if len(raw) >= 3 && rePorcelain.MatchString(raw) {
			x, y := raw[0], raw[1]
			file := raw[3:]
			if raw[:2] == "??" {
				untracked++
				untrackedFiles = append(untrackedFiles, file)
				continue
			}
			if strings.Contains("MADRC", string(x)) {
				staged++
				stagedFiles = append(stagedFiles, file)
			} else if x == 'U' {
				conflicts++
			}
			if y == 'M' || y == 'D' {
				modified++
				modifiedFiles = append(modifiedFiles, file)
			}
			continue
		}
		if m := reLongMatch.FindStringSubmatch(raw); m != nil {
			kind, path := m[1], strings.TrimSpace(m[2])
			switch kind {
			case "both modified":
				conflicts++
			case "modified", "deleted":
				modified++
				modifiedFiles = append(modifiedFiles, path)
			case "new file", "renamed":
				staged++
				stagedFiles = append(stagedFiles, path)
			}
			continue
		}
	}

	var out strings.Builder
	if branch != "" {
		fmt.Fprintf(&out, "* %s\n", branch)
	}
	if staged > 0 {
		fmt.Fprintf(&out, "+ Staged: %d files\n", staged)
		for _, f := range stagedFiles[:min(len(stagedFiles), StatusMaxFiles)] {
			fmt.Fprintf(&out, "   %s\n", f)
		}
		if len(stagedFiles) > StatusMaxFiles {
			fmt.Fprintf(&out, "   ... +%d more\n", len(stagedFiles)-StatusMaxFiles)
		}
	}
	if modified > 0 {
		fmt.Fprintf(&out, "~ Modified: %d files\n", modified)
		for _, f := range modifiedFiles[:min(len(modifiedFiles), StatusMaxFiles)] {
			fmt.Fprintf(&out, "   %s\n", f)
		}
		if len(modifiedFiles) > StatusMaxFiles {
			fmt.Fprintf(&out, "   ... +%d more\n", len(modifiedFiles)-StatusMaxFiles)
		}
	}
	if untracked > 0 {
		fmt.Fprintf(&out, "? Untracked: %d files\n", untracked)
		for _, f := range untrackedFiles[:min(len(untrackedFiles), StatusMaxUntracked)] {
			fmt.Fprintf(&out, "   %s\n", f)
		}
		if len(untrackedFiles) > StatusMaxUntracked {
			fmt.Fprintf(&out, "   ... +%d more\n", len(untrackedFiles)-StatusMaxUntracked)
		}
	}
	if conflicts > 0 {
		fmt.Fprintf(&out, "conflicts: %d files\n", conflicts)
	}
	if staged == 0 && modified == 0 && untracked == 0 && conflicts == 0 {
		out.WriteString("clean — nothing to commit\n")
	}
	return strings.TrimRight(out.String(), "\n")
}

// gitLog compacts git log output.
func gitLog(input string) string {
	lines := strings.Split(input, "\n")
	if len(lines) > GitLogMaxLines {
		lines = lines[:GitLogMaxLines]
		lines = append(lines, fmt.Sprintf("... (%d more commits)", len(strings.Split(input, "\n"))-GitLogMaxLines))
	}
	return strings.Join(lines, "\n")
}

// buildOutput compacts build tool output.
func buildOutput(input string) string {
	lines := strings.Split(input, "\n")
	if len(lines) == 0 {
		return input
	}
	var errors, warnings, deprecations []string
	var summary string
	compilingCount := 0
	downloadingCount := 0
	inCargoError := false
	reCargoErrCont := regexp.MustCompile(`^\s*(-->|\||\d+\s*\|=)`)

	reNpmErr := regexp.MustCompile(`(?i)^npm (ERR!|error)`)
	reYarnErr := regexp.MustCompile(`(?i)^yarn error`)
	reNpmWarnDep := regexp.MustCompile(`(?i)^npm warn deprecated`)
	reNpmWarn := regexp.MustCompile(`(?i)^npm warn`)
	reYarnWarn := regexp.MustCompile(`(?i)^yarn warn`)
	reErrorPrefix := regexp.MustCompile(`(?i)^error(\[|:)`)
	reWarningPrefix := regexp.MustCompile(`(?i)^warning(\[|:)`)
	reErrorBracket := regexp.MustCompile(`(?i)^\[ERROR\]`)
	reBuildFailed := regexp.MustCompile(`(?i)^BUILD FAILED`)
	reWarningBracket := regexp.MustCompile(`(?i)^\[WARNING\]`)
	reCompiling := regexp.MustCompile(`(?i)^\s*Compiling\s+\S+`)
	reDownloading := regexp.MustCompile(`(?i)^\s*Downloading\s+\S+|^Fetching\s+`)
	reSummary := regexp.MustCompile(`(?i)^(added|removed|changed|audited|installed)\s+\d+\s+package|^\s*Finished\s+|^BUILD SUCCESS|^\d+\s+(vulnerabilities|packages?|warnings?|errors?)|^Successfully (installed|built)|^To address .* issues|^Run ` + "`" + `npm (audit|fund)` + "`" + `|packages are looking for funding`)

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		if inCargoError {
			if trimmed == "" {
				inCargoError = false
				continue
			}
			if reCargoErrCont.MatchString(line) {
				errors = append(errors, line)
				continue
			}
			inCargoError = false
		}

		if reNpmErr.MatchString(trimmed) || reYarnErr.MatchString(trimmed) {
			errors = append(errors, line)
			continue
		}
		if reNpmWarnDep.MatchString(trimmed) {
			deprecations = append(deprecations, line)
			continue
		}
		if reNpmWarn.MatchString(trimmed) || reYarnWarn.MatchString(trimmed) {
			warnings = append(warnings, line)
			continue
		}
		if reErrorPrefix.MatchString(trimmed) || strings.HasPrefix(trimmed, "error -->") {
			errors = append(errors, line)
			inCargoError = true
			continue
		}
		if reWarningPrefix.MatchString(trimmed) || strings.HasPrefix(trimmed, "warning -->") {
			warnings = append(warnings, line)
			inCargoError = true
			continue
		}
		if reErrorBracket.MatchString(trimmed) || reBuildFailed.MatchString(trimmed) {
			errors = append(errors, line)
			continue
		}
		if reWarningBracket.MatchString(trimmed) {
			warnings = append(warnings, line)
			continue
		}
		if reCompiling.MatchString(trimmed) {
			compilingCount++
			continue
		}
		if reDownloading.MatchString(trimmed) {
			downloadingCount++
			continue
		}
		if reSummary.MatchString(trimmed) {
			if summary != "" {
				summary += "\n" + line
			} else {
				summary = line
			}
			continue
		}
	}

	var out strings.Builder
	keepDep := min(len(deprecations), 3)
	for i := 0; i < keepDep; i++ {
		fmt.Fprintln(&out, deprecations[i])
	}
	if len(deprecations) > 3 {
		fmt.Fprintf(&out, "... +%d more deprecated packages\n", len(deprecations)-3)
	}
	if compilingCount > 0 {
		fmt.Fprintf(&out, "Compiled %d packages\n", compilingCount)
	}
	if downloadingCount > 0 {
		fmt.Fprintf(&out, "Downloaded %d packages\n", downloadingCount)
	}
	for _, e := range errors {
		fmt.Fprintln(&out, e)
	}
	keepWarnings := min(len(warnings), 5)
	for i := 0; i < keepWarnings; i++ {
		fmt.Fprintln(&out, warnings[i])
	}
	if len(warnings) > 5 {
		fmt.Fprintf(&out, "... +%d more warnings\n", len(warnings)-5)
	}
	if summary != "" {
		fmt.Fprintln(&out, summary)
	}
	return strings.TrimRight(out.String(), "\n")
}

// grep keeps grep output as-is (Rust takes first 10 matches per file; we mirror
// by returning the input unchanged but the compressor uses safeApply).
func grep(input string) string {
	lines := strings.Split(input, "\n")
	if len(lines) <= GrepPerFileMax {
		return input
	}
	return strings.Join(lines[:GrepPerFileMax], "\n") + fmt.Sprintf("\n... (%d more grep matches)", len(lines)-GrepPerFileMax)
}

// find keeps find output with a cap.
func find(input string) string {
	lines := strings.Split(input, "\n")
	if len(lines) <= FindTotalDirMax {
		return input
	}
	return strings.Join(lines[:FindTotalDirMax], "\n") + fmt.Sprintf("\n... (%d more paths)", len(lines)-FindTotalDirMax)
}

// ls compacts ls output.
func ls(input string) string {
	lines := strings.Split(input, "\n")
	out := []string{}
	extCounts := map[string]int{}
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasPrefix(line, "total ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 9 {
			out = append(out, line)
			continue
		}
		name := fields[len(fields)-1]
		out = append(out, name)
		ext := ""
		if i := strings.LastIndex(name, "."); i >= 0 {
			ext = name[i:]
		}
		if ext != "" {
			extCounts[ext]++
		}
	}
	type extCount struct {
		ext   string
		count int
	}
	var ecs []extCount
	for e, c := range extCounts {
		ecs = append(ecs, extCount{e, c})
	}
	sort.Slice(ecs, func(i, j int) bool {
		if ecs[i].count == ecs[j].count {
			return ecs[i].ext < ecs[j].ext
		}
		return ecs[i].count > ecs[j].count
	})
	if len(ecs) > 0 {
		top := ecs[:min(len(ecs), LsExtSummaryTop)]
		parts := []string{}
		for _, ec := range top {
			parts = append(parts, fmt.Sprintf("%s:%d", ec.ext, ec.count))
		}
		out = append(out, "", "[ext summary: "+strings.Join(parts, ", ")+"]")
	}
	return strings.Join(out, "\n")
}

// tree compacts tree output.
func tree(input string) string {
	lines := strings.Split(input, "\n")
	if len(lines) <= TreeMaxLines {
		return input
	}
	return strings.Join(lines[:TreeMaxLines], "\n") + fmt.Sprintf("\n... (%d more tree lines)", len(lines)-TreeMaxLines)
}

// dedupLog collapses duplicate adjacent lines and blank streaks.
func dedupLog(input string) string {
	lines := strings.Split(input, "\n")
	out := []string{}
	var prev *string
	runCount := 0
	blankStreak := 0

	flushRun := func() {
		if prev != nil && runCount > 1 {
			out = append(out, fmt.Sprintf("  ... (%d duplicate lines)", runCount-1))
		}
	}

	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			if blankStreak < 1 {
				out = append(out, line)
			}
			blankStreak++
			flushRun()
			prev = nil
			runCount = 0
			continue
		}
		blankStreak = 0
		if prev != nil && *prev == line {
			runCount++
			continue
		}
		flushRun()
		out = append(out, line)
		prev = &line
		runCount = 1
		if len(out) >= DedupLineMax {
			out = append(out, fmt.Sprintf("... (truncated at %d lines)", DedupLineMax))
			return strings.Join(out, "\n")
		}
	}
	flushRun()
	return strings.Join(out, "\n")
}

// smartTruncate keeps head + tail.
func smartTruncate(input string) string {
	lines := strings.Split(input, "\n")
	if len(lines) < SmartTruncateMinLines {
		return input
	}
	head := lines[:SmartTruncateHead]
	tail := lines[len(lines)-SmartTruncateTail:]
	cut := len(lines) - len(head) - len(tail)
	return strings.Join(append(append(head, fmt.Sprintf("... +%d lines truncated", cut)), tail...), "\n")
}

// readNumbered compacts line-numbered file dumps.
func readNumbered(input string) string {
	lines := strings.Split(input, "\n")
	if len(lines) < SmartTruncateMinLines {
		return input
	}
	head := lines[:SmartTruncateHead]
	tail := lines[len(lines)-SmartTruncateTail:]
	cut := len(lines) - len(head) - len(tail)
	return strings.Join(append(append(head, fmt.Sprintf("... +%d numbered lines truncated", cut)), tail...), "\n")
}

// searchList compacts search-list output.
func searchList(input string) string {
	lines := strings.Split(input, "\n")
	if len(lines) <= SearchListTotalDirMax {
		return input
	}
	return strings.Join(lines[:SearchListTotalDirMax], "\n") + fmt.Sprintf("\n... (%d more search results)", len(lines)-SearchListTotalDirMax)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
