package boss

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/namejiahui/mcp-boss-zp/internal/config"
)

func newTestGuard(t *testing.T, minSec, maxSec, limit int) (*Guard, func(time.Duration)) {
	t.Helper()
	cfg := &config.AppConfig{
		StateFile:       filepath.Join(t.TempDir(), "state.json"),
		GreetMinSec:     minSec,
		GreetMaxSec:     maxSec,
		GreetDailyLimit: limit,
	}
	g := NewGuard(cfg)
	base := time.Date(2026, 11, 18, 10, 0, 0, 0, time.Local)
	offset := time.Duration(0)
	g.now = func() time.Time { return base.Add(offset) }
	return g, func(d time.Duration) { offset += d }
}

func TestGuardRateLimit(t *testing.T) {
	g, advance := newTestGuard(t, 10, 10, 100)

	if err := g.BeforeGreet(); err != nil {
		t.Fatalf("首次打招呼应放行: %v", err)
	}
	g.AfterGreet()

	advance(3 * time.Second)
	err := g.BeforeGreet()
	if err == nil || !strings.Contains(err.Error(), "过于频繁") {
		t.Fatalf("间隔不足时应被拒绝, got: %v", err)
	}

	advance(8 * time.Second)
	if err := g.BeforeGreet(); err != nil {
		t.Fatalf("间隔满足后应放行: %v", err)
	}
}

func TestGuardDailyLimit(t *testing.T) {
	g, _ := newTestGuard(t, 0, 0, 2)

	for i := 0; i < 2; i++ {
		if err := g.BeforeGreet(); err != nil {
			t.Fatalf("第 %d 次应放行: %v", i+1, err)
		}
		g.AfterGreet()
	}

	err := g.BeforeGreet()
	if err == nil || !strings.Contains(err.Error(), "上限") {
		t.Fatalf("超过每日配额应被拒绝, got: %v", err)
	}
}

func TestGuardCooldownEscalation(t *testing.T) {
	g, advance := newTestGuard(t, 0, 0, 100)

	err := g.RegisterSecurityCheck()
	if err == nil || !strings.Contains(err.Error(), "5 分钟") {
		t.Fatalf("首次安全验证应冷却 5 分钟, got: %v", err)
	}

	if err := g.Check(); err == nil {
		t.Fatal("冷却期内 Check 应返回错误")
	}

	advance(6 * time.Minute)
	if err := g.Check(); err != nil {
		t.Fatalf("冷却结束后应放行: %v", err)
	}

	advance(2 * time.Minute)
	if err := g.RegisterSecurityCheck(); err == nil || !strings.Contains(err.Error(), "20 分钟") {
		t.Fatalf("15 分钟内第二次应升级为 20 分钟, got: %v", err)
	}

	advance(1 * time.Minute)
	if err := g.RegisterSecurityCheck(); err == nil || !strings.Contains(err.Error(), "60 分钟") {
		t.Fatalf("15 分钟内第三次应升级为 60 分钟, got: %v", err)
	}
}

func TestGuardPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	cfg := &config.AppConfig{
		StateFile:       path,
		GreetMinSec:     0,
		GreetMaxSec:     0,
		GreetDailyLimit: 1,
	}

	g := NewGuard(cfg)
	g.now = func() time.Time { return time.Date(2026, 11, 18, 10, 0, 0, 0, time.Local) }
	if err := g.BeforeGreet(); err != nil {
		t.Fatalf("首次应放行: %v", err)
	}
	g.AfterGreet()

	// 模拟进程重启：同一状态文件重新加载
	g2 := NewGuard(cfg)
	g2.now = g.now
	err := g2.BeforeGreet()
	if err == nil || !strings.Contains(err.Error(), "上限") {
		t.Fatalf("重启后应继承已用完的配额, got: %v", err)
	}
}
