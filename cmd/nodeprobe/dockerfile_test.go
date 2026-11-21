package main

import (
	"os"
	"strings"
	"testing"
)

// The image's command lines must never contain "/etc/node": Synology's Security Advisor freezes
// (SIGSTOP) every process whose command line does, watchdog and worker alike (see the Dockerfile).
func TestDockerfileCommandLines(t *testing.T) {
	b, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	var cmds int
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || (f[0] != "CMD" && f[0] != "ENTRYPOINT" && f[0] != "HEALTHCHECK") {
			continue
		}
		cmds++
		if strings.Contains(line, "/etc/node") {
			t.Errorf("a command line with /etc/node: %s", line)
		}
	}
	if cmds == 0 {
		t.Error("no ENTRYPOINT, CMD or HEALTHCHECK found")
	}
	if !strings.HasPrefix(defaultConfig, "/etc/nodeprobe/") {
		t.Errorf("the image mounts its config at /etc/nodeprobe, the default is %s", defaultConfig)
	}
}
