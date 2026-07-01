// Package killer hard-kills a process, simulating a power loss or `kill -9`:
// no graceful shutdown handler runs, the process just stops.
//
// os.Process.Kill maps to SIGKILL on Unix and TerminateProcess on Windows —
// exactly the abrupt termination the resilience harness needs, with no
// opportunity for the server to flush or clean up.
package killer

import "os"

// Kill terminates the process abruptly.
func Kill(p *os.Process) error {
	return p.Kill()
}
