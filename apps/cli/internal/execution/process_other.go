//go:build !unix

package execution

import (
	"os/exec"
	"time"
)

func configureProcess(command *exec.Cmd) {
	command.WaitDelay = 250 * time.Millisecond
}
