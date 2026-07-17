package rtk

// Constants ported from open-sse/rtk/constants.js.
const (
	RawCap                 = 10 * 1024 * 1024
	MinCompressSize        = 500
	DetectWindow           = 1024
	GitDiffHunkMaxLines    = 100
	GitDiffContextKeep     = 3
	GitLogMaxLines         = 200
	DedupLineMax           = 2000
	GrepPerFileMax         = 10
	FindPerDirMax          = 10
	FindTotalDirMax        = 20
	StatusMaxFiles         = 10
	StatusMaxUntracked     = 10
	LsExtSummaryTop        = 5
	TreeMaxLines           = 200
	SearchListPerDirMax    = 10
	SearchListTotalDirMax  = 20
	SmartTruncateHead      = 120
	SmartTruncateTail      = 60
	SmartTruncateMinLines  = 250
	ReadNumberedMinHitRatio = 0.7
)

// Filter names (Rust parity + JS extras).
const (
	FilterGitDiff       = "git-diff"
	FilterGitStatus     = "git-status"
	FilterGitLog        = "git-log"
	FilterGrep          = "grep"
	FilterFind          = "find"
	FilterLs            = "ls"
	FilterTree          = "tree"
	FilterDedupLog      = "dedup-log"
	FilterSmartTruncate = "smart-truncate"
	FilterReadNumbered  = "read-numbered"
	FilterSearchList    = "search-list"
	FilterBuildOutput   = "build-output"
)

// Noise directories for ls filter.
var LsNoiseDirs = []string{
	"node_modules", ".git", "target", "__pycache__",
	".next", "dist", "build", ".cache", ".turbo",
	".vercel", ".pytest_cache", ".mypy_cache", ".tox",
	".venv", "venv",
	"env",
	"coverage", ".nyc_output", ".DS_Store", "Thumbs.db",
	".idea", ".vscode", ".vs", "*.egg-info", ".eggs",
}
