//go:build !windows

package browser

// MaximizeHostWindow 在非 Windows 平台不做窗口操作
func MaximizeHostWindow() bool {
	return false
}

// HostWindowMaximized 非 Windows 平台恒为 false
func HostWindowMaximized() bool {
	return false
}
