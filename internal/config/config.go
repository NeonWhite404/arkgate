// Package config 处理 ArkGate 的运行时配置（环境变量 + 默认值）。
package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Conf 是全局运行时配置的单例。
var Conf = &Config{Timeouts: &Timeouts{}}

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

type Config struct {
	// Web 管理监听地址。
	ListenAddr string
	// 数据文件目录（含 sqlite、secret.key）。默认 ~/.arkgate。
	DataDir string
	// 上游火山方舟 Base URL。
	ArkBaseURL string

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

func getenvInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
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
