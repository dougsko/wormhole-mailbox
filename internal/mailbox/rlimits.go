package mailbox

import (
	"fmt"
	"syscall"
)

const desiredRLimitNoFile = 10000

// IncreaseRLimits raises the soft RLIMIT_NOFILE to at least desiredRLimitNoFile
// if possible. Best effort: failures are logged, not fatal.
func IncreaseRLimits() {
	var rlim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rlim); err != nil {
		fmt.Printf("unable to read RLIMIT_NOFILE: %v\n", err)
		return
	}
	if rlim.Cur >= desiredRLimitNoFile {
		return
	}
	target := rlim.Max
	if target > desiredRLimitNoFile {
		target = desiredRLimitNoFile
	}
	if target <= rlim.Cur {
		return
	}
	rlim.Cur = target
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rlim); err != nil {
		fmt.Printf("unable to raise RLIMIT_NOFILE: %v (leaving at %d)\n", err, rlim.Cur)
		return
	}
	fmt.Printf("RLIMIT_NOFILE soft=%d hard=%d\n", rlim.Cur, rlim.Max)
}
