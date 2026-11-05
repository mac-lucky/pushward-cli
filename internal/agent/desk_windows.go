package agent

import (
	"context"
	"fmt"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procGetLastInputInfo = windows.NewLazySystemDLL("user32.dll").NewProc("GetLastInputInfo")

// SystemIdle reads how long Windows has had no key press or mouse input.
// Over SSH there is no local input to read, so it is nil and the user counts
// as away.
func SystemIdle() Idler {
	if os.Getenv("SSH_CONNECTION") != "" {
		return nil
	}
	return func(context.Context) (time.Duration, error) {
		// Call panics where user32.dll or the function is missing (Nano
		// Server).
		if err := procGetLastInputInfo.Find(); err != nil {
			return 0, err
		}
		// LASTINPUTINFO: its own size, then the tick count of the last input.
		info := struct{ size, tick uint32 }{}
		info.size = uint32(unsafe.Sizeof(info))
		// #nosec G103 -- GetLastInputInfo fills the struct it points to
		if ok, _, err := procGetLastInputInfo.Call(uintptr(unsafe.Pointer(&info))); ok == 0 {
			return 0, fmt.Errorf("GetLastInputInfo: %w", err)
		}
		// DurationSinceBoot is GetTickCount64; cut to the 32 bits of the
		// last input's count.
		now := uint32(windows.DurationSinceBoot().Milliseconds()) // #nosec G115 -- truncated on purpose
		return sinceTick(now, info.tick), nil
	}
}
