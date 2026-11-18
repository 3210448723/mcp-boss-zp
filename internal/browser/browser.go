package browser

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

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

	return p, nil
}

// PageForBoss 优先复用已存在的 BOSS 推荐页标签页，没有才新建
// 返回值 reused 表示是否复用了用户已有的标签页（复用页在退出时不会被关闭）
func (bm *BrowserManager) PageForBoss() (*rod.Page, bool, error) {
	b, err := bm.EnsureBrowser()
	if err != nil {
		return nil, false, err
	}

	if bm.cfg.ReuseBossTab {
		if p, url := bm.findBossJobPage(b); p != nil {
			log.Printf("[Browser] 复用已存在的 BOSS 标签页: %s", url)
			bm.FitWindow(p)
			return p, true, nil
		}
	}

	p, err := bm.NewStealthPage()
	if err != nil {
		return nil, false, err
	}
	return p, false, nil
}

// findBossJobPage 在现有标签页里查找 BOSS 推荐页
func (bm *BrowserManager) findBossJobPage(b *rod.Browser) (*rod.Page, string) {
	pages, err := b.Pages()
	if err != nil {
		return nil, ""
	}
	for _, p := range pages {
		info, infoErr := p.Info()
		if infoErr != nil || info == nil {
			continue
		}
		if strings.Contains(info.URL, "zhipin.com/web/geek/job") {
			return p, info.URL
		}
	}
	return nil, ""
}

// FitWindow 修正浏览器窗口/视口。只在接管模式下处理：
// 自启动模式的窗口尺寸本身由我们控制，不需要也不应该动它。
func (bm *BrowserManager) FitWindow(p *rod.Page) {
	if !bm.cfg.ViewportFit || !bm.IsExternal() {
		return
	}

	metrics, ok := bm.readMetrics(p)
	if !ok || metrics.ScreenW <= 0 {
		return
	}
	if float64(metrics.InnerW) >= float64(metrics.ScreenW)*0.9 {
		return // 内容区已经铺满屏幕，不做任何操作
	}

	// 首选系统层最大化：真实窗口铺满后页面布局自然正确
	if bm.cfg.MaximizeWindow {
		if HostWindowMaximized() {
			log.Println("[Browser] Edge 窗口已处于最大化状态，跳过视口覆盖")
			return
		}
		if MaximizeHostWindow() {
			log.Println("[Browser] 已最大化 Edge 窗口，页面随窗口铺满屏幕")
			return
		}
	}

	// 兜底：窗口无法最大化时才使用视口覆盖
	bm.NormalizeViewport(p)
}

// NormalizeViewport 修正视口：内容区远小于屏幕时，按屏幕尺寸铺满并保持 dpr 与真实缩放一致
// 注意必须在页面加载完成之后调用，导航会重置 Emulation 覆盖
func (bm *BrowserManager) NormalizeViewport(p *rod.Page) {
	if !bm.cfg.ViewportFit {
		return
	}

	metrics, ok := bm.readMetrics(p)
	if !ok || metrics.ScreenW <= 0 {
		return
	}

	// 内容区已接近屏幕宽度时不做任何改动
	if float64(metrics.InnerW) >= float64(metrics.ScreenW)*0.9 {
		return
	}

	desiredW := metrics.ScreenW
	desiredH := metrics.ScreenH - 64 // 扣掉标签栏/工具栏占用的高度
	log.Printf("[Browser] 检测到视口异常 (inner=%dx%d, screen=%dx%d)，自动铺满屏幕", metrics.InnerW, metrics.InnerH, metrics.ScreenW, metrics.ScreenH)

	factor := 1.0
	for attempt := 0; attempt < 3; attempt++ {
		w := int(math.Round(float64(desiredW) * factor))
		h := int(math.Round(float64(desiredH) * factor))
		_ = p.SetViewport(&proto.EmulationSetDeviceMetricsOverride{
			Width:             w,
			Height:            h,
			DeviceScaleFactor: 1,
		})
		time.Sleep(500 * time.Millisecond)

		cur, curOK := bm.readMetrics(p)
		if !curOK || cur.InnerW <= 0 {
			return
		}
		if abs(cur.InnerW-desiredW) <= 16 && abs(cur.InnerH-desiredH) <= 16 {
			log.Printf("[Browser] 视口已修正为 %dx%d (dpr=%.2f)", cur.InnerW, cur.InnerH, cur.DPR)
			return
		}
		factor *= float64(desiredW) / float64(cur.InnerW)
	}
}

type pageMetrics struct {
	InnerW  int     `json:"iw"`
	InnerH  int     `json:"ih"`
	ScreenW int     `json:"sw"`
	ScreenH int     `json:"sh"`
	DPR     float64 `json:"dpr"`
}

func (bm *BrowserManager) readMetrics(p *rod.Page) (pageMetrics, bool) {
	const js = `() => JSON.stringify({iw: window.innerWidth, ih: window.innerHeight, sw: screen.width, sh: screen.height, dpr: window.devicePixelRatio})`
	tp := p.Timeout(5 * time.Second)
	val, err := tp.Eval(js)
	if err != nil {
		return pageMetrics{}, false
	}
	var m pageMetrics
	if err := json.Unmarshal([]byte(val.Value.Str()), &m); err != nil {
		return pageMetrics{}, false
	}
	return m, true
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
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
