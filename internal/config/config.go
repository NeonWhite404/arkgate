// Package config 处理 ArkGate 的运行时配置（环境变量 + 默认值）。
package config

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Conf 是全局运行时配置的单例。
var Conf = &Config{Timeouts: &Timeouts{}, Currency: &Currency{}}

// Timeouts 上游超时的运行时可调值：管理端（设置页）写、网关每次请求读，
// 因此用原子存取而不是普通字段——避免热改与转发并发读写产生数据竞争。
// 单位纳秒；0 表示关闭该项超时。
type Timeouts struct {
	request    atomic.Int64
	firstToken atomic.Int64
}

// Request 非流式请求的整体超时（含读完响应体）。0 = 不限。
func (t *Timeouts) Request() time.Duration { return time.Duration(t.request.Load()) }

// FirstToken 流式请求的首字节超时（首字节前失败可换叶子重试）。0 = 关闭。
func (t *Timeouts) FirstToken() time.Duration { return time.Duration(t.firstToken.Load()) }

// SetRequest / SetFirstToken 由管理端热改；负值归零（视为关闭）。
func (t *Timeouts) SetRequest(d time.Duration) { t.request.Store(int64(max0(d))) }

func (t *Timeouts) SetFirstToken(d time.Duration) { t.firstToken.Store(int64(max0(d))) }

func max0(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d
}

// Currency 展示币种与汇率。
//
// 设计取舍（重要）：**存储始终保持美元**。
//   - 单价直接来自 LiteLLM 目录（美元），目录补全不需换算；
//   - 历史日志里的 cost/input_cost/... 全是美元，换币种不需回填；
//   - 汇率只是**展示参数**，改汇率不会污染已落库数据，也不会让
//     「同一个数字在执行不同汇率的不同时刻看起来不一致」。
//
// 反面做法是把人民币写进库里：一旦汇率调整，历史数字到底是哪个汇率下的
// 就无法区分了，且目录补全/成本回填/迁移全要改。
//
// Rate 为 0 表示不换算（仍按美元展示）。
type Currency struct {
	rate   atomic.Uint64 // math.Float64bits，避免浮点原子不可用
	symbol atomic.Value  // string
	code   atomic.Value  // string
}

// Rate 返回汇率（1 美元 = Rate 本币）。0 = 不换算。
func (c *Currency) Rate() float64 { return math.Float64frombits(c.rate.Load()) }

// Symbol 返回货币符号（如 "¥"）；Code 返回 ISO 代码（如 "CNY"）。
func (c *Currency) Symbol() string {
	if v, ok := c.symbol.Load().(string); ok && v != "" {
		return v
	}
	return "$"
}

func (c *Currency) Code() string {
	if v, ok := c.code.Load().(string); ok && v != "" {
		return v
	}
	return "USD"
}

// Set 由管理端热改。rate <= 0 视为不换算（回落美元），负值不做特殊语义。
func (c *Currency) Set(rate float64, symbol, code string) {
	if rate < 0 {
		rate = 0
	}
	c.rate.Store(math.Float64bits(rate))
	c.symbol.Store(symbol)
	c.code.Store(code)
}

// Convert 把美元金额换算为展示币种。rate=0 时原值返回（不换算）。
func (c *Currency) Convert(usd float64) float64 {
	r := c.Rate()
	if r <= 0 {
		return usd
	}
	return usd * r
}

type Config struct {
	// Web 管理监听地址。
	ListenAddr string
	// 数据文件目录（含 sqlite、secret.key）。默认 ~/.arkgate。
	DataDir string
	// 上游火山方舟 Base URL。
	ArkBaseURL string

	// Currency 计价币种：单价与成本仍以美元存储（与上游目录一致），
	// 仅在展示层按汇率换算。管理端可热改。
	Currency *Currency

	// 访问令牌（若设置，管理 API 与 UI 需带此 token）。
	AccessToken string

	// 代理轮询等默认参数。
	DefaultCircuitMax   time.Duration
	MaxRetriesAvailable int

	// Timeouts 上游超时（非流式整体超时 + 流式首字节超时）：
	// 启动时取「DB 持久化值 > 环境变量 > 内置默认」，之后可在设置页热改。
	Timeouts *Timeouts

	// 会话粘性 TTL：同一子 Key + 模型在该时长内的后续请求固定路由到同一叶节点
	// （利于上游 prompt cache 命中）。0 表示关闭。默认 5min。
	SessionTTL time.Duration
}

// 上游超时的内置默认值（环境变量未设置时生效）。
const (
	DefaultRequestTimeout    = 300 * time.Second
	DefaultFirstTokenTimeout = 30 * time.Second
)

// Load 从环境变量加载配置，未设置的项回落到默认值。
func Load() *Config {
	Conf.ListenAddr = getenv("ARKGATE_ADDR", "0.0.0.0:8002")
	Conf.DataDir = getenv("ARKGATE_DATA_DIR", defaultDataDir())
	Conf.ArkBaseURL = strings.TrimRight(getenv("ARKGATE_BASE_URL", "https://ark.cn-beijing.volces.com/api/v3"), "/")
	Conf.AccessToken = os.Getenv("ARKGATE_TOKEN")

	Conf.DefaultCircuitMax = 60 * time.Second
	Conf.MaxRetriesAvailable = 3

	if Conf.Timeouts == nil {
		Conf.Timeouts = &Timeouts{}
	}
	if Conf.Currency == nil {
		Conf.Currency = &Currency{}
	}
	// 计价币种：默认美元（不换算）。汇率由管理端设置页填写并落库，
	// 启动时 admin.New 会用 DB 值覆盖，故此处只需保证零值安全。
	Conf.Currency.Set(getFloat("ARKGATE_CURRENCY_RATE", 0),
		getenv("ARKGATE_CURRENCY_SYMBOL", "$"), getenv("ARKGATE_CURRENCY_CODE", "USD"))
	Conf.Timeouts.SetRequest(getDurationSec("ARKGATE_REQUEST_TIMEOUT", DefaultRequestTimeout))
	Conf.Timeouts.SetFirstToken(getDurationSec("ARKGATE_FIRST_TOKEN_TIMEOUT", DefaultFirstTokenTimeout))
	Conf.SessionTTL = getDurationSec("ARKGATE_SESSION_TTL", 5*time.Minute)

	return Conf
}

// getDurationSec 读取以秒为单位的时长环境变量（支持小数）；0 或负值返回 0（关闭）。
func getDurationSec(k string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f <= 0 {
		return 0
	}
	return time.Duration(f * float64(time.Second))
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// getFloat 读取浮点环境变量；未设置或解析失败返回 def。
// 与 getDurationSec 不同，这里不把 0 当作「关闭」——汇率 0 本身就是
// 「不换算」的合法值，所以解析成功就直接返回。
func getFloat(k string, def float64) float64 {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		return def
	}
	return f
}

// defaultDataDir 默认把运行时数据放在可执行文件同级目录，
// 避免污染用户主目录等其它位置。ARKGATE_DATA_DIR 可显式覆盖。
func defaultDataDir() string {
	if exe, err := os.Executable(); err == nil {
		if dir, err := filepath.Abs(filepath.Dir(exe)); err == nil {
			return dir
		}
	}
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}
