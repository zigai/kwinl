package main

import "testing"

func TestVersionText(t *testing.T) {
	oldVersion, oldCommit, oldDirty := version, commit, dirty

	t.Cleanup(func() {
		version, commit, dirty = oldVersion, oldCommit, oldDirty
	})

	for _, tt := range []struct {
		name    string
		version string
		commit  string
		dirty   string
		want    string
	}{
		{
			name:    "release",
			version: "1.2.3",
			commit:  "abc1234",
			dirty:   "false",
			want:    "kwinl 1.2.3 (abc1234)",
		},
		{
			name:    "dirty",
			version: "v1.2.3-dirty",
			commit:  "abc1234",
			dirty:   "true",
			want:    "kwinl v1.2.3-dirty (abc1234, dirty)",
		},
		{
			name:    "development",
			version: "dev",
			commit:  "none",
			dirty:   "false",
			want:    "kwinl dev (none)",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			version, commit, dirty = tt.version, tt.commit, tt.dirty

			if got := versionText(); got != tt.want {
				t.Fatalf("versionText() = %q, want %q", got, tt.want)
			}
		})
	}
}
