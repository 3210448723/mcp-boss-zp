package boss

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/namejiahui/mcp-boss-zp/internal/config"
)

// guardState 是需要跨进程保留的风控状态（进程重启后仍然生效）
type guardState struct {
	GreetDate         string  `json:"greet_date"`
	GreetCount        int     `json:"greet_count"`
	LastGreetUnix     int64   `json:"last_greet_unix"`
	SecurityChecks    []int64 `json:"security_checks"`
	CooldownUntilUnix int64   `json:"cooldown_until_unix"`
}

// Guard 负责速率控制、每日配额与风控冷却，所有状态落盘保存
type Guard struct {
	mu          sync.Mutex
	path        string
	st          guardState
	minInterval time.Duration
	maxInterval time.Duration
	dailyLimit  int
	rnd         *rand.Rand
	now         func() time.Time
}

// NewGuard 依据配置创建守卫，并读取上次遗留的状态
func NewGuard(cfg *config.AppConfig) *Guard {
	g := &Guard{
		path:        cfg.StateFile,
		minInterval: time.Duration(cfg.GreetMinSec) * time.Second,
		maxInterval: time.Duration(cfg.GreetMaxSec) * time.Second,
		dailyLimit:  cfg.GreetDailyLimit,
		rnd:         rand.New(rand.NewSource(time.Now().UnixNano())),
		now:         time.Now,
	}
	g.load()
	return g
}

func (g *Guard) load() {
	if g.path == "" {
		return
	}
	b, err := os.ReadFile(g.path)
	if err != nil {
		return
	}
	_ = json.Unmarshal(b, &g.st)
}

func (g *Guard) saveLocked() {
	if g.path == "" {
		return
	}
	b, err := json.MarshalIndent(g.st, "", "  ")
	if err != nil {
		return
	}
	dir := filepath.Dir(g.path)
	_ = os.MkdirAll(dir, 0755)
	tmp := g.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0644); err != nil {
		return
	}
	_ = os.Rename(tmp, g.path)
}

// Check 调用前的通用检查（冷却期内直接拒绝）
func (g *Guard) Check() error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if until := g.st.CooldownUntilUnix; until > 0 {
		if remain := time.Unix(until, 0).Sub(g.now()); remain > 0 {
			return fmt.Errorf("风控冷却中，请 %d 分钟后再试（触发安全验证后自动冷却）", int(remain.Minutes())+1)
		}
	}
	return nil
}

// RegisterSecurityCheck 记录一次安全验证页，并按频次升级冷却时长
func (g *Guard) RegisterSecurityCheck() error {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.now()
	// 只保留最近 15 分钟内的记录
	cut := now.Add(-15 * time.Minute).Unix()
	kept := make([]int64, 0, len(g.st.SecurityChecks)+1)
	for _, t := range g.st.SecurityChecks {
		if t >= cut {
			kept = append(kept, t)
		}
	}
	kept = append(kept, now.Unix())
	g.st.SecurityChecks = kept

	var cooldown time.Duration
	switch n := len(kept); {
	case n >= 3:
		cooldown = 60 * time.Minute
	case n == 2:
		cooldown = 20 * time.Minute
	default:
		cooldown = 5 * time.Minute
	}
	g.st.CooldownUntilUnix = now.Add(cooldown).Unix()
	g.saveLocked()

	return fmt.Errorf("页面跳转到安全验证，已暂停 %d 分钟；请在弹出的 BOSS 标签页里手动完成验证后再试", int(cooldown.Minutes()))
}

// BeforeGreet 打招呼前的频率与配额检查
func (g *Guard) BeforeGreet() error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if until := g.st.CooldownUntilUnix; until > 0 {
		if remain := time.Unix(until, 0).Sub(g.now()); remain > 0 {
			return fmt.Errorf("风控冷却中，请 %d 分钟后再试", int(remain.Minutes())+1)
		}
	}

	now := g.now()
	today := now.Format("2006-01-02")
	if g.st.GreetDate != today {
		g.st.GreetDate = today
		g.st.GreetCount = 0
	}

	if g.dailyLimit > 0 && g.st.GreetCount >= g.dailyLimit {
		g.saveLocked()
		return fmt.Errorf("今日打招呼已达上限 %d 个，请明天再继续（可在 env 中调整 BOSS_GREET_DAILY_LIMIT）", g.dailyLimit)
	}

	if g.st.LastGreetUnix > 0 {
		// 每次随机取一个间隔，天然形成不规则的节奏
		interval := g.minInterval
		if span := g.maxInterval - g.minInterval; span > 0 {
			interval += time.Duration(g.rnd.Int63n(int64(span)))
		}
		if elapsed := now.Sub(time.Unix(g.st.LastGreetUnix, 0)); elapsed < interval {
			wait := interval - elapsed
			return fmt.Errorf("打招呼过于频繁，需再等 %d 秒（当前间隔要求 %d-%d 秒）", int(wait.Seconds())+1,
				int(g.minInterval.Seconds()), int(g.maxInterval.Seconds()))
		}
	}
	return nil
}

// AfterGreet 成功打招呼后记账
func (g *Guard) AfterGreet() {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.now()
	today := now.Format("2006-01-02")
	if g.st.GreetDate != today {
		g.st.GreetDate = today
		g.st.GreetCount = 0
	}
	g.st.GreetCount++
	g.st.LastGreetUnix = now.Unix()
	g.saveLocked()
}
