package boss

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/namejiahui/mcp-boss-zp/internal/browser"
)

var (
	//go:embed scripts/boss.js
	bossScript string

	jobIdRegexp = regexp.MustCompile(`/job_detail/([a-zA-Z0-9~_-]+)\.html`)
)

const (
	bossJobURL = "https://www.zhipin.com/web/geek/job"
)

type BossService struct {
	bm    *browser.BrowserManager
	guard *Guard
	page  *rod.Page
	mu    sync.Mutex
}

func NewBossService(bm *browser.BrowserManager, guard *Guard) *BossService {
	return &BossService{bm: bm, guard: guard}
}

// isSecurityPage 判断当前是否被 BOSS 风控带到安全验证页
func isSecurityPage(u string) bool {
	if u == "" {
		return false
	}
	lower := strings.ToLower(u)
	return strings.Contains(lower, "_security_check") ||
		strings.Contains(lower, "safe/verify") ||
		strings.Contains(lower, "/security-check")
}

// ensurePageValid 确保 BOSS 直聘页面有效且处于可用状态
func (s *BossService) ensurePageValid() (*rod.Page, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.page != nil {
		info, err := s.page.Info()
		if err == nil {
			if isSecurityPage(info.URL) {
				return nil, s.registerSecurityCheck()
			}
			return s.page, nil
		}
		log.Println("[BossService] 检测到 BOSS 标签页已关闭或失联，正在重新打开...")
	}

	p, reused, err := s.bm.PageForBoss()
	if err != nil {
		return nil, fmt.Errorf("初始化 BOSS 页面失败: %w", err)
	}
	s.page = p

	if reused {
		log.Printf("[BossService] 已复用现有 BOSS 标签页")
	}

	// 只有当前不在推荐页时才导航，复用标签页时避免多余刷新
	currentInfo, err := s.page.Info()
	if err != nil || !strings.Contains(currentInfo.URL, "/web/geek/job") {
		log.Printf("[BossService] 正在打开 BOSS 直聘推荐页: %s", bossJobURL)
		if err := s.page.Navigate(bossJobURL); err != nil {
			return nil, fmt.Errorf("页面导航失败: %w", err)
		}
		_ = s.page.WaitLoad()
	}

	// 窗口/视口修正必须放在加载完成之后：导航会重置 Emulation 覆盖
	// 注意该便携 Edge 的真实 OS 窗口会被锁在 1280x800，必须走系统层最大化
	s.bm.FitWindow(s.page)

	info, err := s.page.Info()
	if err != nil {
		return nil, fmt.Errorf("获取页面信息失败: %w", err)
	}

	if isSecurityPage(info.URL) {
		return nil, s.registerSecurityCheck()
	}

	if strings.Contains(info.URL, "/web/user") || strings.Contains(info.URL, "login") {
		log.Println("==================================================================")
		log.Println("【BOSS直聘 MCP】检测到您尚未登录或登录已失效！")
		log.Println("【BOSS直聘 MCP】请在弹出的浏览器窗口中扫码登录 BOSS 直聘。")
		log.Println("【BOSS直聘 MCP】登录成功后，系统将自动保持会话并就绪...")
		log.Println("==================================================================")

		timeout := time.After(3 * time.Minute)
		ticker := time.NewTicker(1500 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-timeout:
				return nil, fmt.Errorf("等待扫码登录超时 (3分钟)，请重启服务重新扫码")
			case <-ticker.C:
				currentInfo, err := s.page.Info()
				if err == nil && isSecurityPage(currentInfo.URL) {
					return nil, s.registerSecurityCheck()
				}
				if err == nil && !strings.Contains(currentInfo.URL, "/web/user") && !strings.Contains(currentInfo.URL, "login") {
					log.Println("【BOSS直聘 MCP】检测到登录成功！会话已自动持久化。")
					return s.page, nil
				}
			}
		}
	}

	log.Println("[BossService] BOSS 直聘已处于有效登录状态。")
	return s.page, nil
}

// executeInPage 在 BOSS 页面上下文中执行脚本并返回字符串
func (s *BossService) executeInPage(jsFn string, args ...any) (string, error) {
	page, err := s.ensurePageValid()
	if err != nil {
		return "", err
	}

	// 页面在首次导航后可能发生客户端重定向，导致执行上下文失效 (-32000)。
	// 此处做有限重试，避免把一次性竞态直接抛给调用方。
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		val, err := page.Eval(jsFn, args...)
		if err == nil {
			return val.Value.Str(), nil
		}
		lastErr = err
		if !isContextLost(err) {
			return "", err
		}
		// 若重定向后已落到安全验证页，立即停手，绝不反复冲击验证页
		if info, infoErr := page.Info(); infoErr == nil && isSecurityPage(info.URL) {
			return "", s.registerSecurityCheck()
		}
		_ = page.WaitLoad()
		time.Sleep(humanJitter(400, 900))
	}

	return "", lastErr
}

// isContextLost 判断是否为页面导航导致的执行上下文失效
func isContextLost(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "-32000") ||
		strings.Contains(msg, "navigated") ||
		strings.Contains(msg, "Execution context was destroyed")
}

// humanJitter 返回 [minMs, maxMs] 区间内的随机延迟，避免固定节奏
func humanJitter(minMs, maxMs int) time.Duration {
	if maxMs <= minMs {
		return time.Duration(minMs) * time.Millisecond
	}
	return time.Duration(minMs+rand.Intn(maxMs-minMs+1)) * time.Millisecond
}

// registerSecurityCheck 记录一次安全验证并进入冷却
func (s *BossService) registerSecurityCheck() error {
	if s.guard == nil {
		return fmt.Errorf("页面跳转到 BOSS 安全验证，请手动完成验证后重试")
	}
	return s.guard.RegisterSecurityCheck()
}

// moveMouseHuman 用分段插值 + 轻微抖动模拟人工鼠标移动，最后停顿再点击
func (s *BossService) moveMouseHuman(page *rod.Page, target proto.Point) {
	start := proto.NewPoint(
		target.X+float64(rand.Intn(241)-120),
		target.Y+float64(rand.Intn(161)-80),
	)
	steps := 3 + rand.Intn(2)
	for i := 1; i <= steps; i++ {
		t := float64(i) / float64(steps)
		eased := t * t * (3 - 2*t) // ease-in-out
		x := start.X + (target.X-start.X)*eased + float64(rand.Intn(5)-2)
		y := start.Y + (target.Y-start.Y)*eased + float64(rand.Intn(5)-2)
		if x < 1 {
			x = 1
		}
		if y < 1 {
			y = 1
		}
		_ = page.Mouse.MoveTo(proto.NewPoint(x, y))
		time.Sleep(humanJitter(12, 35))
	}
	time.Sleep(humanJitter(120, 320))
}

// GetVisibleJobs 读取当前页面上渲染的可见职位列表
func (s *BossService) GetVisibleJobs(ctx context.Context) ([]VisibleJob, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	if s.guard != nil {
		if err := s.guard.Check(); err != nil {
			return nil, err
		}
	}

	rawJSON, err := s.executeInPage(bossScript, "getJobs")
	if err != nil {
		return nil, fmt.Errorf("读取页面职位列表失败: %w", err)
	}

	var jobs []VisibleJob
	if err := json.Unmarshal([]byte(rawJSON), &jobs); err != nil {
		return nil, fmt.Errorf("解析职位列表失败: %w, 原始内容: %s", err, rawJSON)
	}

	return jobs, nil
}

// extractJobIdFromCard 从职位卡片节点中提取唯一 targetJobId (使用 Guard Clause 卫语句扁平化)
func extractJobIdFromCard(card *rod.Element) string {
	link, err := card.Element("a[href*='/job_detail/']")
	if err != nil || link == nil {
		return ""
	}

	href, err := link.Attribute("href")
	if err != nil || href == nil {
		return ""
	}

	m := jobIdRegexp.FindStringSubmatch(*href)
	if len(m) <= 1 {
		return ""
	}

	return m[1]
}

// GreetJob 使用事件驱动 (MutationObserver) + Rod 原生仿真硬件点击 (极简通信、通知式响应、无轮询)
func (s *BossService) GreetJob(ctx context.Context, cardIndex int) (*GreetResult, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	if cardIndex <= 0 {
		return nil, fmt.Errorf("卡片序号必须从 1 开始")
	}

	if s.guard != nil {
		if err := s.guard.Check(); err != nil {
			return nil, err
		}
	}

	page, err := s.ensurePageValid()
	if err != nil {
		return nil, err
	}

	// 1. 调用 bossScript 的 getCardElement 获取第 N 个卡片句柄
	targetCard, err := page.ElementByJS(rod.Eval(bossScript, "getCardElement", cardIndex).ByObject().ByPromise())
	if err != nil || targetCard == nil {
		return nil, fmt.Errorf("未找到序号为 %d 的职位卡片: %v", cardIndex, err)
	}

	// 提取该卡片的 targetJobId (用于右侧详情对齐校验)
	targetJobId := extractJobIdFromCard(targetCard)

	// 2. 真实硬件鼠标点击左侧卡片，触发右侧详情栏加载
	if err := targetCard.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return nil, fmt.Errorf("点击第 %d 个职位卡片失败: %w", cardIndex, err)
	}

	// 3. 【通知式等待】：由浏览器内核的 MutationObserver 事件直接驱动决议 Promise！
	// 将 targetJobId 纯字符串传给 JS，JS 纯粹做事件通知与 jobId 对齐核验
	btn, err := page.ElementByJS(rod.Eval(bossScript, "waitForChatButton", targetJobId, 8000).ByObject().ByPromise())
	if err != nil || btn == nil {
		return nil, fmt.Errorf("在序号 %d 的职位上未找到沟通按钮 (可能已被抢先沟通或加载超时)", cardIndex)
	}

	// 4. 检查按钮文案，若已沟通过则不再重复打招呼 (可打断点看 btnText)
	btnText, _ := btn.Text()
	btnText = strings.TrimSpace(btnText)
	if strings.Contains(btnText, "继续") || strings.Contains(btnText, "已沟通") || strings.Contains(btnText, "聊过") {
		return &GreetResult{
			Success: true,
			Message: "该职位此前已发送过沟通邀请，无需重复打招呼",
		}, nil
	}

	// 5. 真实物理鼠标点击【立即沟通】
	// 点击前先做频率/配额检查，避免"读了页面却打不出去"或反向的无效计数
	if s.guard != nil {
		if err := s.guard.BeforeGreet(); err != nil {
			return nil, err
		}
	}

	// 通过 Shape 计算物理中心坐标并直接触发鼠标事件，跳过 Rod 默认的 WaitInteractable 遮挡死循环
	if shape, err := btn.Shape(); err == nil && shape != nil {
		box := shape.Box()
		centerPt := proto.NewPoint(box.X+box.Width/2, box.Y+box.Height/2)
		s.moveMouseHuman(page, centerPt)
		if err := page.Mouse.Click(proto.InputMouseButtonLeft, 1); err != nil {
			log.Printf("[BossService] 物理鼠标点击异常 (%v)，使用 DOM 兜底触发", err)
			if _, domErr := page.Eval(`(el) => el.click()`, btn); domErr != nil {
				return nil, fmt.Errorf("点击【立即沟通】按钮失败: %w", domErr)
			}
		}
	} else {
		// 兜底方案：直接通过 DOM click 触发
		if _, err := page.Eval(`(el) => el.click()`, btn); err != nil {
			return nil, fmt.Errorf("点击【立即沟通】按钮失败: %w", err)
		}
	}

	if s.guard != nil {
		s.guard.AfterGreet()
	}

	return &GreetResult{
		Success: true,
		Message: "已在页面上成功点击【立即沟通】按钮！",
	}, nil
}
