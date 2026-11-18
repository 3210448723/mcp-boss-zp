//go:build windows

package browser

import (
	"log"
	"strings"
	"syscall"
	"unsafe"
)

const (
	swMaximize              = 3
	smCXScreen              = 0
	smCYScreen              = 1
	processQueryLimitedInfo = 0x1000
)

var (
	user32                       = syscall.NewLazyDLL("user32.dll")
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procEnumWindows              = user32.NewProc("EnumWindows")
	procIsWindowVisible          = user32.NewProc("IsWindowVisible")
	procGetWindowTextW           = user32.NewProc("GetWindowTextW")
	procGetClassNameW            = user32.NewProc("GetClassNameW")
	procGetWindowThreadProcessID = user32.NewProc("GetWindowThreadProcessId")
	procShowWindow               = user32.NewProc("ShowWindow")
	procIsZoomed                 = user32.NewProc("IsZoomed")
	procGetWindowRect            = user32.NewProc("GetWindowRect")
	procGetSystemMetrics         = user32.NewProc("GetSystemMetrics")
	procOpenProcess              = kernel32.NewProc("OpenProcess")
	procQueryFullProcessImage    = kernel32.NewProc("QueryFullProcessImageNameW")
	procCloseHandle              = kernel32.NewProc("CloseHandle")
)

type winRect struct {
	Left, Top, Right, Bottom int32
}

// MaximizeHostWindow 在系统层最大化承载 BOSS 页面的浏览器窗口。
// 部分“反检测指纹”版便携浏览器会锁定真实窗口尺寸，CDP 的 setWindowBounds
// 只会返回 state=maximized 而不真正改变窗口，因此只能在 OS 层操作。
func MaximizeHostWindow() bool {
	hwnd, w, h, ok, zoomed := findEdgeWindow()
	if !ok || zoomed {
		return false
	}
	procShowWindow.Call(uintptr(hwnd), swMaximize)
	log.Printf("[Browser] 已在系统层最大化 Edge 窗口 (窗口 %dx%d -> 屏幕 %dx%d)", w, h, systemMetric(smCXScreen), systemMetric(smCYScreen))
	return true
}

// HostWindowMaximized 承载 BOSS 页面的 Edge 窗口是否已经处于最大化状态
func HostWindowMaximized() bool {
	_, _, _, ok, zoomed := findEdgeWindow()
	return ok && zoomed
}

// findEdgeWindow 定位 Edge 主窗口（优先标题含 BOSS 的那个）
func findEdgeWindow() (syscall.Handle, int32, int32, bool, bool) {
	var fallback syscall.Handle
	var target syscall.Handle
	candidates := 0
	var targetW, targetH int32

	cb := syscall.NewCallback(func(hwnd syscall.Handle, _ uintptr) uintptr {
		if r, _, _ := procIsWindowVisible.Call(uintptr(hwnd)); r == 0 {
			return 1
		}
		if windowClassName(hwnd) != "Chrome_WidgetWin_1" {
			return 1
		}
		// 必须锁定 Edge 进程：豆包客户端等 Electron 应用的窗口类名同样是
		// Chrome_WidgetWin_1，仅靠类名会把它们的窗口也一起最大化
		if !strings.Contains(strings.ToLower(processImageName(hwnd)), "msedge") {
			return 1
		}
		w, h := windowSize(hwnd)
		if w <= 0 || h <= 0 {
			return 1
		}

		candidates++
		fallback = hwnd

		if title := windowText(hwnd); strings.Contains(title, "BOSS") || strings.Contains(title, "直聘") ||
			strings.Contains(title, "zhipin") {
			target = hwnd
			targetW, targetH = w, h
		}
		return 1
	})

	if r, _, _ := procEnumWindows.Call(cb, 0); r == 0 {
		return 0, 0, 0, false, false
	}

	// 没有标题匹配且存在多个 Edge 窗口时，宁可不操作，避免动到用户的其它窗口
	if target == 0 {
		if candidates != 1 {
			log.Printf("[Browser] 找到 %d 个 Edge 窗口且无 BOSS 标题匹配，跳过窗口操作", candidates)
			return 0, 0, 0, false, false
		}
		target = fallback
		targetW, targetH = windowSize(fallback)
	}

	zoomed := false
	if z, _, _ := procIsZoomed.Call(uintptr(target)); z != 0 {
		zoomed = true
	}
	return target, targetW, targetH, true, zoomed
}

// processImageName 取窗口所属进程的可执行文件名（如 msedge.exe）
func processImageName(hwnd syscall.Handle) string {
	var pid uint32
	procGetWindowThreadProcessID.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&pid)))
	if pid == 0 {
		return ""
	}

	h, _, _ := procOpenProcess.Call(processQueryLimitedInfo, 0, uintptr(pid))
	if h == 0 {
		return ""
	}
	defer procCloseHandle.Call(h)

	buf := make([]uint16, 512)
	size := uint32(len(buf))
	r, _, _ := procQueryFullProcessImage.Call(h, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
	if r == 0 {
		return ""
	}

	full := syscall.UTF16ToString(buf[:size])
	if i := strings.LastIndexAny(full, `\/`); i >= 0 {
		return full[i+1:]
	}
	return full
}

func windowSize(hwnd syscall.Handle) (int32, int32) {
	var rect winRect
	procGetWindowRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&rect)))
	return rect.Right - rect.Left, rect.Bottom - rect.Top
}

func windowText(hwnd syscall.Handle) string {
	buf := make([]uint16, 512)
	r, _, _ := procGetWindowTextW.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if r == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf)
}

func windowClassName(hwnd syscall.Handle) string {
	buf := make([]uint16, 256)
	procGetClassNameW.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return syscall.UTF16ToString(buf)
}

func systemMetric(index int) int {
	r, _, _ := procGetSystemMetrics.Call(uintptr(index))
	return int(int32(r))
}
