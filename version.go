package main

import (
	"fmt"
	"runtime/debug"
)

const shortCommitLen = 7

func versionText() string {
	v, c, modified := version, commit, dirty

	if info, ok := debug.ReadBuildInfo(); ok {
		if v == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
			v = info.Main.Version
		}

		for _, setting := range info.Settings {
			switch {
			case setting.Key == "vcs.revision" && c == "none":
				c = setting.Value[:min(shortCommitLen, len(setting.Value))]
			case setting.Key == "vcs.modified" && modified == "":
				modified = setting.Value
			}
		}
	}

	if modified == "true" {
		c += ", dirty"
	}

	return fmt.Sprintf("kwinl %s (%s)", v, c)
}
