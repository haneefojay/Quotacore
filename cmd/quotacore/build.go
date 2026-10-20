package main

import "runtime/debug"

// buildVersion is the release, set at link time with
// -ldflags "-X main.buildVersion=v0.1.0"; a development build says so.
var buildVersion = "0.1.0-dev"

// buildCommit is the VCS revision the toolchain stamped into the binary, so a
// running binary is traceable to a source tree (GET /v1/admin/status).
func buildCommit() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				return s.Value
			}
		}
	}
	return "unknown"
}
