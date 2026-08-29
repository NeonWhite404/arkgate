# ArkGate

**多供应商多账号负载均衡 + OpenAI 兼容网关**。

把火山方舟、OpenAI 以及任意 OpenAI 兼容供应商（DeepSeek / Moonshot / vLLM / OpenRouter…）
放进一个网关，解决两大痛点：

1. **多账号负载均衡**——把多家供应商、十几个账号的 API Key 放进池子里，网关在请求时按
   **平滑加权轮询 + 熔断 + 限流** 自动选一个账号，对下游完全透明。
2. **易读模型名 ↔ 上游模型标识映射**——火山账号的接入点是 `ep-` 开头的随机串、账号间互不相同，
   其他供应商的模型 ID 也长短不一。网关维护「**账号 × 易读模型名 → 上游模型标识**」的映射表，
   下游用 `doubao-seed-1-6` 这样的名字调用，网关转发时自动换回真实标识。

> 纯 Go 编写，单二进制、零外部依赖（SQLite 内置、前端 `go:embed` 内嵌），开箱即用。
> 上游调用复用 **openai-go 官方 SDK**，请求参数原样透传，跟随官方版本升级。

---

## 特性

- **OpenAI 兼容**：
  - `POST /v1/chat/completions`（流式/非流式）
  - `POST /v1/responses`（流式/非流式，原生透传）
  - `POST /v1/images/generations`（文本到图像生成）
  - `GET /v1/models`
  可直接把 `base_url` 指到本网关，用任意 OpenAI SDK 调用。
- **多供应商**：账号可归属 `ark` / `openai` / `custom`（任意 OpenAI 兼容方言，账号自带 base URL）；
  供应商差异是一张数据表，新增供应商零代码。
- **熔断 + 限流（叶节点级）**：限流与熔断只发生在「账号 × 模型标识」的元组（叶节点）上——
  每个接入点可独立配置**并发 / RPM / TPM**；连续失败自动熔断该接入点（指数退避冷却）并切到别的叶子。
  账号本身不单独限流、不单独熔断，只做 active/disabled 开关。
- **能力感知路由**：`/v1/responses`、`/v1/images/generations` 只路由到支持该能力的账号
  （供应商默认能力 × 账号级三态覆盖：继承/强制可用/强制禁用）。
- **模态感知 fallback**：模型分文本/图像两类，fallback 链只在同类型内退避；
  文本模型打图像接口会被明确拒绝。
- **子 Key 体系**：真实上游 Key 只存服务端（加密落盘），下发给客户端的是 `sk-xxx` 子 Key。
  子 Key 可限制可访问模型、可访问账号、当日 Token 限额与当日图像张数限额（图像限额只约束图像请求）。
- **用量统计（子 Key 维度）**：不向上游同步，含请求次数、token、图像张数、耗时、错误；
  日限额按 `usage_daily` 表按自然日真实计量。账号与接入点也分别累计，熔断/限流状态实时可见。
- **内嵌 Web 管理界面**：Arco Design 风格，总览 / 上游账号 / 模型映射 / 子 Key / 日志 / 使用说明。

## 快速开始

```bash
# 构建
go build -o arkgate .

# 运行
./arkgate
```

首次运行自动生成管理访问令牌并打印到控制台，登录 Web 界面时使用。
默认监听 `0.0.0.0:8002`，端口被占用时自动向后递增直到找到可用端口。

运行时数据（SQLite 数据库 + 加密密钥）保存在可执行文件同级目录，不污染系统其它位置。
可通过 `ARKGATE_DATA_DIR` 环境变量显式覆盖。

### 环境变量

| 变量 | 说明 | 默认值 |
|---|---|---|
| `ARKGATE_ADDR` | 监听地址 | `0.0.0.0:8002`（被占用时自动向后递增） |
| `ARKGATE_DATA_DIR` | 数据目录（sqlite + secret.key） | 可执行文件同级目录 |
| `ARKGATE_SECRET` | 固定加密主密钥（可选，不设则自动生成） | — |

## 使用流程

1. **添加账号**：在「上游账号」选择供应商（火山方舟 / OpenAI / 自定义），填入该供应商的
   API Key；自定义供应商需填 base URL。Key 为任意字符串，网关不做格式假设。
2. **建模型目录**：在「模型映射」新建易读模型名（如 `doubao-seed-1-6`、`seedream-image`），
   并选择类型（文本 / 图像）。
3. **添加映射**：为「账号 × 模型」填写上游模型标识（Ark 填 `ep-xxx`，OpenAI 填 `gpt-4o` 等）。
4. **建子 Key**：创建一张 `sk-xxx`，可选限制模型/账号/当日 Token/当日图像张数限额。
5. **调用**：把客户端 `base_url` 指向本网关，用子 Key 作为 API Key：

```bash
curl http://localhost:8002/v1/chat/completions \
  -H "Authorization: Bearer sk-你的子Key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "doubao-seed-1-6",
    "messages": [{"role": "user", "content": "你好"}],
    "stream": false
  }'
```

图像生成（模型类型需为 image，`size` 等参数按上游语法传参，网关不翻译）：

```bash
curl http://localhost:8002/v1/images/generations \
  -H "Authorization: Bearer sk-你的子Key" \
  -H "Content-Type: application/json" \
  -d '{"model": "seedream-image", "prompt": "a cat", "n": 2}'
```

> 映射同时是元组级流控的配置点：每个 `(账号 × 模型)` 映射可独立设置
> 权重 / 并发 / RPM / TPM。网关对某个模型做路由时，只在该模型的**可用元组集合**里选
> （未熔断、未限流、账号启用、账号具备该 API 的能力），权重可作为元组权重或回落到账号权重。

## 流控与路由模型

采用**树状结构**：

```
账号（父节点）——供应商 + Key + active/disabled 开关 + 统计聚合，不单独限流、不单独熔断
   ├── 映射（叶节点：账号 × 上游模型标识）——限流（并发/RPM/TPM）与熔断只在这一层
   ├── 映射（叶节点）
   └── ...
```

- **路由**：客户端用子 Key 鉴权 + 指定易读模型名 → 网关收集该模型下所有可用叶节点
  （账号启用、叶子未熔断、未超限、能力匹配）→ 平滑加权轮询（权重：`叶.weight > 账号.weight > 1`）
  → 命中 `(账号, 模型标识)` → 用账号真实 Key 替换子 Key、把 `model` 换成上游标识后转发。
- **限流**：并发 / RPM / TPM 全部在**叶节点**上生效，一个账号的不同接入点互不影响；
  TPM 单位即「计费单位」（文本 = token，图像 = 张数）。
- **熔断**：只在叶节点上发生——连续失败 5 次即熔断该接入点（1s → 最长 60s 指数退避冷却）。
- **fallback 链**：模型可配置有序 fallback（仅限同类型已存在的模型）；某模型全部元组不可用时
  按链退避；请求失败也会排除当前叶换下一个重试，最多 3 次（流式为单次尝试）。
- **请求重试**：转发失败把当前叶节点加入本请求排除集，切下一个叶子重试，最多 3 次。

## 并发模型

Go `net/http` 每个请求一个 goroutine，转发链路全程无全局串行锁：

- 元组/账号运行态（并发数、连续失败、熔断时间、RPM/TPM 窗口）用**原子操作 + 细粒度锁**维护；
- 负载均衡 `Select` 走无阻塞快照 + 短临界区；
- 用量统计与日志写库经**有界 channel 异步落盘**，不阻塞请求热路径。

## 数据安全

- 上游 API Key（任意字符串）用 **AES-256-GCM** 加密后落盘（主密钥存 `DataDir/secret.key`，
  权限 `0600`），任何 API 响应都不回传明文；
- 子 Key 明文只存一份（用于展示给用户复制），鉴权按 SHA-256 哈希匹配；
- 管理 API 采用访问令牌鉴权（Bearer Token），令牌哈希存储。

## 目录结构

```
arkgate/
├── main.go                 # 入口：装配 + 路由（/v1、/api、/ 内嵌前端）
├── internal/
│   ├── config/             # 环境变量配置
│   ├── secure/             # AES-256-GCM 加解密
│   ├── model/              # 数据模型 + 滑动窗口
│   ├── store/              # SQLite 持久化（幂等迁移，只增不改）
│   ├── provider/           # 供应商注册表 + openai-go SDK 骨干 + SSE 字节转发
│   ├── balancer/           # 加权轮询 + 熔断 + 限流 + 能力过滤
│   ├── gateway/            # /v1 OpenAI 兼容转发层（chat/responses/images）
│   └── admin/              # /api 管理接口 + 令牌鉴权
├── web/                    # 内嵌单页前端（go:embed）
└── docs/                   # 设计文档
```

## 测试

```bash
go test ./...
```

## 设计文档

多供应商与图像生成的架构设计、实施决策与验证记录见
[docs/design-multi-provider.md](docs/design-multi-provider.md)。
