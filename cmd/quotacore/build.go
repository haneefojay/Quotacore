package main

import "runtime/debug"

// buildVersion is the release, set at link time with
// -ldflags "-X main.buildVersion=v0.1.0"; a development build says so.
var buildVersion = "0.1.0-dev"

// buildRevision is the source revision, set at link time by the image build
// (-X main.buildRevision=<sha>), whose context has no .git for the toolchain
// to stamp from (IP-15 D2). Empty means the toolchain's stamp is used.
var buildRevision = ""

// buildCommit is the VCS revision the toolchain stamped into the binary, so a
// running binary is traceable to a source tree (GET /v1/admin/status).
func buildCommit() string {
	if buildRevision != "" {
		return buildRevision
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				return s.Value
			}
		}
	}
	return "unknown"
}
