package version

import (
	"runtime"
	"strings"
	"testing"
)

// A binary found on a host has to be able to say which commit produced it,
// so every stamped value must survive into the string.
func TestStringCarriesEveryStamp(t *testing.T) {
	previous := [3]string{Version, Commit, Date}
	t.Cleanup(func() { Version, Commit, Date = previous[0], previous[1], previous[2] })

	Version, Commit, Date = "v1.2.3", "abc1234", "2026-08-21T09:00:00Z"

	got := String()
	for _, want := range []string{"v1.2.3", "abc1234", "2026-08-21T09:00:00Z", runtime.Version(), runtime.GOOS} {
		if !strings.Contains(got, want) {
			t.Errorf("String() = %q, want it to contain %q", got, want)
		}
	}
	if strings.Contains(got, "\n") {
		t.Errorf("String() = %q, want a single line", got)
	}
}

func TestUnstampedBuildSaysSo(t *testing.T) {
	if !strings.HasPrefix(String(), "dev ") {
		t.Errorf("String() = %q, want it to start with the dev placeholder", String())
	}
}
