//go:build !windows && !unix

package main

import "os/exec"

func configureDetachedCommand(cmd *exec.Cmd) {}

func shouldAutoDetach() bool {
	return false
}
