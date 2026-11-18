package browser

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
	"github.com/go-rod/stealth"
	"github.com/namejiahui/mcp-boss-zp/internal/config"
)

const (
	defaultWindowWidth  = 1440
	defaultWindowHeight = 900
)

// BrowserManager 管理底层 Chrome/Edge 浏览器进程及连接
type BrowserManager struct {
	cfg      *config.AppConfig
	browser  *rod.Browser
	mu       sync.Mutex
	external bool        // 是否复用外部已运行的浏览器（不负责关闭其进程）
	pages    []*rod.Page // 本服务打开的页面，退出时统一关闭
}

// NewBrowserManager 创建通用浏览器管理器
func NewBrowserManager(cfg *config.AppConfig) *BrowserManager {
	return &BrowserManager{
		cfg: cfg,
	}
}

// cleanStaleLocks 清理崩溃残留的 Chrome 锁文件
func cleanStaleLocks(userDataDir string) {
	for _, f := range []string{"SingletonLock", "SingletonSocket", "SingletonCookie"} {
		_ = os.Remove(filepath.Join(userDataDir, f))
	}
}

// resolveCDP 将 "9222" / "127.0.0.1:9222" / "http://127.0.0.1:9222" / "ws://..." 统一解析为 CDP WebSocket 地址
func resolveCDP(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", fmt.Errorf("CDP 地址为空")
	}
	if strings.HasPrefix(v, "ws") {
		return v, nil
	}
	hostPort := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(v, "http://"), "https://"), "/")
	return launcher.ResolveURL(hostPort)
}

// EnsureBrowser 确保底层浏览器已启动并建立连接
func (bm *BrowserManager) EnsureBrowser() (*rod.Browser, error) {
	bm.mu.Lock()
	defer bm.mu.Unlock()

	if bm.browser != nil {
		return bm.browser, nil
	}

	// 复用已运行的浏览器（沿用真实登录态，无需重启浏览器）
	if bm.cfg.CDPURL != "" {
		u, err := resolveCDP(bm.cfg.CDPURL)
		if err != nil {
			return nil, fmt.Errorf("解析 CDP 地址失败 (%s): %w", bm.cfg.CDPURL, err)
		}
		b := rod.New().ControlURL(u)
		if err := b.Connect(); err != nil {
			return nil, fmt.Errorf("连接已运行的浏览器失败 (请确认浏览器以 --remote-debugging-port 启动且端口可达): %w", err)
		}
		log.Printf("[Browser] 已接管正在运行的浏览器: %s", bm.cfg.CDPURL)
		bm.browser = b
		bm.external = true
		return bm.browser, nil
	}

	if err := os.MkdirAll(bm.cfg.UserDataDir, 0755); err != nil {
		return nil, fmt.Errorf("创建用户数据目录失败: %w", err)
	}

	cleanStaleLocks(bm.cfg.UserDataDir)

	l := launcher.New().
		UserDataDir(bm.cfg.UserDataDir).
		Headless(bm.cfg.Headless).
		Leakless(false).
		Set("window-size", fmt.Sprintf("%d,%d", defaultWindowWidth, defaultWindowHeight)).
		Set("disable-blink-features", "AutomationControlled").
		Set("no-default-browser-check").
		Set("no-first-run")

	if bm.cfg.ChromeBin != "" {
		l.Bin(bm.cfg.ChromeBin)
	} else if bin, has := launcher.LookPath(); has {
		log.Printf("[Browser] 检测到系统浏览器: %s", bin)
		l.Bin(bin)
	} else {
		log.Println("[Browser] 未在系统标准路径找到 Chrome/Edge，将自动下载便携版 Chromium...")
	}

	u, err := l.Launch()
	if err != nil {
		return nil, fmt.Errorf("启动 Chrome 失败 (请确认本机已安装 Chrome/Edge): %w", err)
	}

	b := rod.New().ControlURL(u)
	if err := b.Connect(); err != nil {
		return nil, fmt.Errorf("连接浏览器失败: %w", err)
	}

	bm.browser = b
	return bm.browser, nil
}

// NewStealthPage 创建一个开启了 Stealth 反指纹对抗的全新页面
func (bm *BrowserManager) NewStealthPage() (*rod.Page, error) {
	b, err := bm.EnsureBrowser()
	if err != nil {
		return nil, err
	}

	p, err := stealth.Page(b)
	if err != nil {
		return nil, fmt.Errorf("创建 stealth 页面失败: %w", err)
	}

	bm.mu.Lock()
	bm.pages = append(bm.pages, p)
	bm.mu.Unlock()

	// 接管用户已在使用的浏览器时不能强行覆盖视口：
	// Emulation 会让 innerWidth/innerHeight 与真实 outerWidth/outerHeight 互相矛盾，反而成为自动化痕迹。
	if bm.IsExternal() {
		log.Println("[Browser] 接管模式：保持真实窗口尺寸，不做视口覆盖")
		return p, nil
	}

	// 自启动模式下锁定页面视口为标准 1440x900 宽屏，确保页面 DOM 布局完全一致且不折叠
	_ = p.SetViewport(&proto.EmulationSetDeviceMetricsOverride{
		Width:             defaultWindowWidth,
		Height:            defaultWindowHeight,
		DeviceScaleFactor: 1,
	})

	return p, nil
}

// IsExternal 是否处于接管已运行浏览器的模式
func (bm *BrowserManager) IsExternal() bool {
	bm.mu.Lock()
	defer bm.mu.Unlock()
	return bm.external
}

// Close 优雅关闭浏览器进程
func (bm *BrowserManager) Close() error {
	bm.mu.Lock()
	defer bm.mu.Unlock()

	// 先关闭本服务自己打开的页面，避免在用户浏览器里留下残留标签页
	for _, p := range bm.pages {
		_ = p.Close()
	}
	bm.pages = nil

	if bm.browser == nil {
		return nil
	}

	// 外部复用的浏览器只断开连接，绝不关闭用户正在使用的浏览器进程
	if bm.external {
		bm.browser = nil
		return nil
	}

	err := bm.browser.Close()
	bm.browser = nil
	return err
}
