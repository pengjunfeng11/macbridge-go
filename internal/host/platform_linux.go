package host

import (
	"os"
	"syscall"
	"time"
	"unsafe"
)

func statTimes(r map[string]any, s *syscall.Stat_t) {
	r["createdAt"] = nil
	r["changedAt"] = time.Unix(s.Ctim.Sec, s.Ctim.Nsec).UTC().Format(time.RFC3339Nano)
	r["accessedAt"] = time.Unix(s.Atim.Sec, s.Atim.Nsec).UTC().Format(time.RFC3339Nano)
}
func accessTime(s *syscall.Stat_t) time.Time { return time.Unix(s.Atim.Sec, s.Atim.Nsec) }
func canonical(f *os.File) (bool, error) {
	var t syscall.Termios
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&t)))
	if e != 0 {
		return false, e
	}
	return t.Lflag&syscall.ICANON != 0, nil
}
