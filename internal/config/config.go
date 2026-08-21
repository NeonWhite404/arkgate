// Package config 处理 ArkGate 的运行时配置（环境变量 + 默认值）。
package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Conf 是全局运行时配置的单例。
var Conf = &Config{}

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
	RequestTimeout      time.Duration
	DefaultCircuitMax   time.Duration
	MaxRetriesAvailable int
}

// Load 从环境变量加载配置，未设置的项回落到默认值。
func Load() *Config {
	Conf.ListenAddr = getenv("ARKGATE_ADDR", "0.0.0.0:8002")
	Conf.DataDir = getenv("ARKGATE_DATA_DIR", defaultDataDir())
	Conf.ArkBaseURL = strings.TrimRight(getenv("ARKGATE_BASE_URL", "https://ark.cn-beijing.volces.com/api/v3"), "/")
	Conf.AccessToken = os.Getenv("ARKGATE_TOKEN")

	Conf.RequestTimeout = 300 * time.Second
	Conf.DefaultCircuitMax = 60 * time.Second
	Conf.MaxRetriesAvailable = 3

	return Conf
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
