<div align="center">

# deepseek-vision

### 让只会读文字的 DeepSeek，在 CLIProxyAPI 中可靠地理解图片

`deepseek-vision` 是一个面向 **CLIProxyAPI v7** 的原生请求预处理插件。它通过宿主已有的视觉模型读取图片，
把同一 prompt 中的多张图片转换为一份联合视觉分析，再交给 DeepSeek 继续推理。

[![Release](https://img.shields.io/badge/release-v0.2.0-2ea44f)](https://github.com/Zesuy/Plugin-Deepseek-Vision/releases)
[![CI](https://github.com/Zesuy/Plugin-Deepseek-Vision/actions/workflows/ci.yml/badge.svg)](https://github.com/Zesuy/Plugin-Deepseek-Vision/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![CLIProxyAPI](https://img.shields.io/badge/CLIProxyAPI-v7.2.119-5B5BD6)](https://github.com/router-for-me/CLIProxyAPI)
[![Platforms](https://img.shields.io/badge/platforms-6-4C8BF5)](docs/limitations.md)
[![License](https://img.shields.io/github/license/Zesuy/Plugin-Deepseek-Vision)](LICENSE)

**简体中文** · [English](README_EN.md) · [安装](docs/installation.md) · [配置](docs/configuration.md) · [排障](docs/troubleshooting.md)

</div>

---

DeepSeek 文本模型无法直接消费 OpenAI Responses 请求中的 `input_image`。本插件在 CLIProxyAPI 完成鉴权、
别名和最终模型解析后接住目标请求，让视觉模型先理解图片，再用纯文本分析透明替换图片块。DeepSeek 收到
完整的问题和视觉信息，但不会再收到自己无法读取的原始图片。

> [!IMPORTANT]
> 这不是新的代理、模型提供商或协议转换层。插件不配置额外 endpoint 或 API key；模型路由、凭据、协议转换、
> 网络传输、重试与供应商限流都继续由 CLIProxyAPI 负责。

## v0.2.0 有什么

| 能力 | 行为 |
| --- | --- |
| **宿主原生视觉调用** | 通过 `host.model.execute` 使用 CLIProxyAPI 已配置的 `vision_model`、路由和凭据 |
| **三种下游协议** | 原生处理 OpenAI Responses、Chat Completions 和 Anthropic Messages 请求中的图片 |
| **prompt 级多图理解** | 同一消息或工具结果中的图片按顺序一次交给 VLM，保留比较、变化和上下文关系 |
| **透明且原子的改写** | 原图替换为编号标记和一份联合分析；只有全部处理成功后才把请求交给 DeepSeek |
| **全局背压** | `max_inflight_vision_requests` 限制全进程在途视觉任务，多余任务排队而不是被粗暴拒绝 |
| **按需拆批** | 正常多图请求保持完整；只有宿主明确返回 413 时才按原顺序自适应拆分 |
| **缓存与去重** | 请求内合并相同 prompt 组，跨请求复用可配置 TTL LRU 中的派生分析结果 |
| **非视觉模型提示** | 明确告知 DeepSeek 图片已经分析完成且不能直接读图，避免再次调用 `view_image` |
| **稳定配置生命周期** | 空白或无效的编辑不会让插件拒绝注册；保留上一份有效运行时和可配置表单 |
| **完整诊断 trace** | 可选记录原始上下文、分组、VLM 请求/响应、缓存计划及改写结果，用于复杂多轮排障 |

## 实际效果

| 跨轮次图片理解与模型切换 | 从截图提取前端排障线索 |
| --- | --- |
| <img src="docs/assets/full-context-model-switch.png" alt="切换到 DeepSeek 后继续理解历史图片" width="680"> | <img src="docs/assets/frontend-ui-analysis.png" alt="根据截图分析前端按钮排布" width="680"> |
| 切换到 `deepseek-v4-flash` 后，历史中的图片会先转换为视觉上下文，目标模型不需要直接读图。 | 视觉模型识别表格、按钮分组和换行现象，DeepSeek 再结合代码继续定位 CSS。 |

这些截图来自真实会话。调试 trace 中保存的、未经人工改写的视觉模型解释见
[原始视觉解释](docs/examples/trace-vision-output.md)。同一宿主和图片的 A/B 测试中，任务导向提示与低推理视觉请求
把 VLM 阶段从 27.8 秒降至 7.4 秒、从 49.1 秒降至 16.6 秒，同时保留自动图片细节；`detail=low`
虽更快但会漏掉小字和安全弹窗，因此没有采用。

## 工作方式

```mermaid
flowchart LR
    A["Responses、Chat 或 Claude 请求"] --> B["CLIProxyAPI 鉴权、别名与模型解析"]
    B --> C{"协议、路径和最终模型命中？"}
    C -- "否" --> D["宿主原样处理"]
    C -- "是" --> E["扫描可见历史并按 prompt 分组"]
    E --> F["同组图片一次联合 VLM 分析"]
    F --> G{"全部分析和校验成功？"}
    G -- "否" --> H["安全终止，不转发原图"]
    G -- "是" --> I["写入图片标记与联合分析"]
    I --> J["确认请求中不再含图片块"]
    J --> K["DeepSeek 继续推理"]
```

例如，同一条 prompt 中的三张截图通常只产生一次视觉模型调用。插件保留图片顺序和最多 2,000 字符的
关联 prompt，让 VLM 同时说明各图内容、可见文字以及图片之间的关系。改写后的内容类似：

```text
[Image 1 — already analyzed; the target model cannot read this attachment directly]
[Image 2 — already analyzed; the target model cannot read this attachment directly]
[Image 3 — already analyzed; the target model cannot read this attachment directly]

[Vision preprocessing notice: use the supplied analysis and do not reopen these attachments with view_image]
[Images 1, 2, 3 — Joint visual analysis]
<逐图内容、可见文字、差异与关系>
```

VLM 提示词要求忠实转录文字、标记无法辨认的内容、解释多图关系，并把图片和用户上下文中的指令视为
不可信数据。插件还会清理已消费附件对应的 Codex 临时路径，避免非视觉目标模型再次尝试打开图片。

## 支持边界

请求必须命中以下任一路由，并满足最终模型门控：

```text
openai-response + /v1/responses
openai          + /v1/chat/completions
claude          + /v1/messages
final Model ∈ target_models
```

| 场景 | v0.2.0 |
| --- | --- |
| `input[].content[]` 中的 URL / data URI `input_image` | ✅ |
| 数组型 `function_call_output.output[]` 中的 `input_image` | ✅ |
| Chat `messages[].content[]` 中的 `image_url`，包括 tool 消息 | ✅ |
| Claude message 和 `tool_result.content[]` 中的 base64 / URL 图片 | ✅ |
| 字符串型 `function_call_output.output` | ✅ 原样保留 |
| 同一 prompt 多图、请求中可见的历史轮次图片 | ✅ |
| `stream: true` | ✅ 先预处理，再开始响应流 |
| 默认目标 `deepseek-v4-flash` | ✅ 已验收 |
| `deepseek-v4-pro` | ⚠️ 需显式加入并自行验证上游 Responses 可用性 |
| `/v1/responses/compact`、`/v1/messages/count_tokens`、其他模型 | ➡️ 旁路 |
| 仅提供文件 ID 的图片 | ❌ 返回 422 |
| `previous_response_id` 隐藏的服务端历史 | ❌ 插件不可见 |

## 快速开始

当前插件尚未收录到 CLIProxyAPI 官方插件源，需要先从
[GitHub Releases](https://github.com/Zesuy/Plugin-Deepseek-Vision/releases) 下载与 CLIProxyAPI 运行平台匹配的
v0.2.0 ZIP；解压后只有一个动态库。checksum 校验、其他平台示例和升级步骤见
[安装文档](docs/installation.md)。

### Docker 部署

把宿主机插件目录映射到容器内 `/CLIProxyAPI/plugins`：

```yaml
volumes:
  - /path/to/plugins:/CLIProxyAPI/plugins
```

CLIProxyAPI 在容器中运行，因此应按**容器**架构下载 Linux 资产，而不是按宿主桌面系统选择。Linux amd64
容器应把 ZIP 中的文件放到宿主机：

```text
/path/to/plugins/linux/amd64/deepseek-vision.so
```

Linux arm64 容器把 `amd64` 换成 `arm64`。完成后重启 CLIProxyAPI 容器。

### 直接部署

CLIProxyAPI 默认从启动工作目录下的 `plugins` 读取插件。把动态库放入：

```text
plugins/<GOOS>/<GOARCH>/deepseek-vision.<ext>
```

例如 Linux amd64 使用 `plugins/linux/amd64/deepseek-vision.so`，macOS 使用 `.dylib`，Windows 使用
`.dll`。如果配置了 `plugins.dir`，则用该目录替代默认的 `plugins`。安装后重启 CLIProxyAPI。

### 在 Management HTML 中启用

打开 `http://<CLIProxyAPI地址>:<端口>/management.html`，进入插件页面，启用 `deepseek-vision`，只需把
`vision_model` 选择为 CLIProxyAPI 中已经可用的任意视觉模型并保存。默认目标模型已经是
`deepseek-v4-flash`，其他字段首次使用时保持默认值即可；页面显示插件已加载后便可开始使用。

## 配置重点

| 配置项 | 默认值 | 说明 |
| --- | ---: | --- |
| `target_models` | `deepseek-v4-flash` | 需要视觉预处理的最终模型列表 |
| `vision_model` | `gpt-5.6-luna` | CLIProxyAPI 中已有的视觉模型名称 |
| `language` | `zh` | `zh`、`en` 或 `auto` |
| `max_inflight_vision_requests` | `4` | 全局在途 prompt 组数量，范围 1–16 |
| `emergency_max_images_per_request` | `256` | 极端请求的唯一图片兜底上限，不是日常批大小 |
| `request_timeout_seconds` | `120` | 包含排队时间的整次预处理期限 |
| `analysis_cache_size` | `128` | 派生文本缓存条目；`0` 关闭跨请求缓存 |
| `analysis_cache_ttl_seconds` | `900` | data URI 分析缓存秒数 |
| `analysis_url_cache_ttl_seconds` | `120` | URL 图片分析缓存秒数 |
| `trace_enabled` | `false` | 完整明文调试 trace，仅临时启用 |

### 可选：手动写入配置

不使用 Management HTML 时，最小 YAML 配置只需要启用插件并指定宿主中已有的视觉模型：

```yaml
plugins:
  enabled: true
  configs:
    deepseek-vision:
      enabled: true
      vision_model: gpt-5.6-luna
```

完整字段、默认值和高级限制见 [`config.example.yaml`](config.example.yaml) 与
[配置参考](docs/configuration.md)。

缓存键由有序图片引用、完整 prompt、视觉模型和规范化语言组成；缓存只保存不可逆哈希键与派生分析文本，
不保存原图或图片引用。重配置或重启会创建新的缓存代际。

## 错误与诊断

对于已经命中支持边界且包含图片的请求，插件采用 fail-closed 行为：

| HTTP | 含义 |
| ---: | --- |
| `400` | Responses JSON 或支持范围内的结构无效 |
| `413` | 请求体、图片引用、ABI 准入或唯一图片应急上限（默认 256）被触发 |
| `422` | 图片来源不受支持，例如只有 `file_id` |
| `502` | 视觉模型失败、超时、响应无效或最终改写校验失败 |

普通 413 会通过宿主 `host.log` 记录 `limit_kind`、实际值、上限和配置代际，不记录请求正文或图片内容。

复杂多轮问题可以临时启用 `trace_enabled: true`。文件写入：

```text
logs/deepseek-vision-trace/events.jsonl
logs/deepseek-vision-trace/requests/<request-bundle>/
```

request bundle 包含完整原始 body、图片 URL / data URI、发现位置、prompt 分组、缓存计划、VLM 请求与响应、
解析结果、改写 body 和最终状态。凭据类 header / metadata 会脱敏，但图片和会话正文是明文；请保护目录权限，
只在复现期间开启，并在诊断结束后关闭和清理。Docker 部署时需要把 `/CLIProxyAPI/logs` 挂载到宿主。

## 构建与发布

原生构建需要 Go 1.26、CGO、平台 C 编译器、Python、Git，以及 `nm`（Linux/macOS）或
`objdump`（Windows）。脚本默认构建当前宿主的 GOOS/GOARCH：

```bash
VERSION=0.2.0 ./scripts/package.sh
./scripts/checksum.sh
```

产物是可复现的 `dist/deepseek-vision_0.2.0_<goos>_<goarch>.zip` 和 `dist/checksums.txt`。
普通提交和 PR 除常规检查外，只构建 Linux amd64 兼容包：

```bash
go test ./...
go test -race ./...
go vet ./...
./scripts/verify-contracts.sh
./scripts/package-smoke.sh
```

在 GitHub Actions 中手动运行 Release workflow 并输入 `0.2.0` 后，它才会在 6 个原生 runner 上全量
构建，聚合 6 个 ZIP 与一份 checksum，并把资产写入 Draft Release；检查无误后再由维护者手动发布。
CI 和发布包
不需要也不会包含真实上游 key。宿主 mock E2E 见 [测试文档](docs/testing.md)。

## 当前限制

- v0.2.0 发布 Linux、macOS、Windows 的 amd64/arm64 资产。CLIProxyAPI 也支持 FreeBSD amd64 动态插件，
  但本版本尚未发布未经 FreeBSD 实机验收的资产。
- 插件只改写精确命中的 Responses、Chat Completions 和 Anthropic Messages 路由。
- 预处理必须在响应流开始前完成，因此 VLM 延迟会增加首字节时间。
- 缓存为进程内缓存，不会在多个 CLIProxyAPI 实例间共享。
- URL 图片会由视觉模型所在上游读取；仍需根据部署设置 DNS、网络出口和 allowlist。
- `deepseek-v4-pro` 不是 v0.2.0 的发布验收目标。

完整边界见 [限制说明](docs/limitations.md) 与 [安全说明](docs/security.md)。

## 文档

| 文档 | 内容 |
| --- | --- |
| [安装与运维](docs/installation.md) | 手动 / Store / Docker 安装、升级和回滚 |
| [完整配置](docs/configuration.md) | 字段、默认值、校验、缓存和 trace |
| [接口契约](docs/contracts.md) | ABI、三种下游协议输入改写与错误契约 |
| [架构说明](docs/architecture.md) | 数据流、模块职责与宿主边界 |
| [安全说明](docs/security.md) | 凭据、网络、提示注入与失败安全 |
| [故障排查](docs/troubleshooting.md) | 注册、配置、413 / 502、trace 与容器权限 |
| [测试与验收](docs/testing.md) | 单元、竞态、打包与宿主 E2E |
| [版本记录](CHANGELOG.md) | 发布内容与已验证边界 |

## 致谢

README 的信息组织与视觉表达参考了 [Anionex/codex-vision-proxy](https://github.com/Anionex/codex-vision-proxy)。
两个项目采用不同的集成方式；本项目专注 CLIProxyAPI v7 原生插件与宿主能力复用。

## License

本项目采用 [MIT License](LICENSE)。

---

<div align="center">

如果这个项目对你有帮助，欢迎点一个 Star ⭐

Made with care by [Zesuy](https://github.com/Zesuy)

</div>
