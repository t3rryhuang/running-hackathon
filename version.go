package main

import (
	"runtime"
	"runtime/debug"
	"time"
)

// commit and buildTime are stamped by the Makefile via -ldflags. When the
// binary is built without them (go run, go test) they fall back to the VCS
// stamps the toolchain embeds, so /version is never a lie.
var (
	commit    = ""
	buildTime = ""
	startedAt = time.Now().UTC()
)

// BuildInfo is what /version reports. It is deliberately free of anything
// sensitive: no DSN, no credentials, no user data - only what is needed to tell
// whether the running deployment matches a given commit.
type BuildInfo struct {
	Commit    string `json:"commit"`
	Dirty     bool   `json:"dirty"`
	BuiltAt   string `json:"built_at"`
	GoVersion string `json:"go_version"`
	Backend   string `json:"backend"`
	StartedAt string `json:"started_at"`
	UptimeSec int64  `json:"uptime_seconds"`
}

func buildInfo(cfg Config) BuildInfo {
	sha, dirty := vcsStamp()
	if commit != "" {
		sha = commit
	}
	backend := "sqlite"
	if cfg.DatabaseURL != "" {
		backend = "postgres"
	}
	return BuildInfo{
		Commit:    sha,
		Dirty:     dirty,
		BuiltAt:   buildTime,
		GoVersion: runtime.Version(),
		Backend:   backend,
		StartedAt: startedAt.Format(time.RFC3339),
		UptimeSec: int64(time.Since(startedAt).Seconds()),
	}
}

func vcsStamp() (sha string, dirty bool) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown", false
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			sha = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if sha == "" {
		sha = "unknown"
	}
	return sha, dirty
}
