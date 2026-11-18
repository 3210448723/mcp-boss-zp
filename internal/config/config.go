package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// AppConfig 保存应用运行时配置
type AppConfig struct {
	UserDataDir     string // Chrome 用户数据目录
	Headless        bool   // 是否无头模式
	ChromeBin       string // 自定义 Chrome/Edge 可执行文件路径
	CDPURL          string // 复用已运行浏览器的 CDP 地址 (如 http://127.0.0.1:9222)，为空则自行启动浏览器
	StateFile       string // 速率/配额/冷却状态的持久化文件
	GreetMinSec     int    // 两次打招呼的最小间隔（秒）
	GreetMaxSec     int    // 两次打招呼的最大间隔（秒）
	GreetDailyLimit int    // 每日打招呼上限
	ViewportFit     bool   // 视口异常（内容区远小于屏幕）时是否自动铺满屏幕
	ReuseBossTab    bool   // 是否复用已存在的 BOSS 推荐页标签页
	MaximizeWindow  bool   // 是否在系统层最大化承载 BOSS 页面的浏览器窗口
}

// GetDefaultUserDataDir 获取跨平台默认的数据持久化目录
func GetDefaultUserDataDir() string {
	if custom := os.Getenv("BOSS_USER_DATA_DIR"); custom != "" {
		return custom
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".mcp-boss-zp", "browser_profile")
}

// GetDefaultStateFile 获取默认的运行时状态文件路径
func GetDefaultStateFile() string {
	if custom := strings.TrimSpace(os.Getenv("BOSS_STATE_FILE")); custom != "" {
		return custom
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".mcp-boss-zp", "state.json")
}

func envInt(name string, def int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	return n
}

func envBool(name string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	if v == "" {
		return def
	}
	return !(v == "0" || v == "false" || v == "no" || v == "off")
}

// LoadConfig 从环境变量加载配置
func LoadConfig() *AppConfig {
	headless := false
	if v := strings.ToLower(os.Getenv("BOSS_HEADLESS")); v == "true" || v == "1" {
		headless = true
	}

	minSec := envInt("BOSS_GREET_MIN_SEC", 45)
	maxSec := envInt("BOSS_GREET_MAX_SEC", 90)
	if maxSec < minSec {
		maxSec = minSec
	}

	return &AppConfig{
		UserDataDir:     GetDefaultUserDataDir(),
		Headless:        headless,
		ChromeBin:       strings.TrimSpace(os.Getenv("CHROME_BIN")),
		CDPURL:          strings.TrimSpace(os.Getenv("BOSS_CDP_URL")),
		StateFile:       GetDefaultStateFile(),
		GreetMinSec:     minSec,
		GreetMaxSec:     maxSec,
		GreetDailyLimit: envInt("BOSS_GREET_DAILY_LIMIT", 30),
		ViewportFit:     envBool("BOSS_VIEWPORT_FIT", true),
		ReuseBossTab:    envBool("BOSS_REUSE_TAB", true),
		MaximizeWindow:  envBool("BOSS_MAXIMIZE", true),
	}
}
