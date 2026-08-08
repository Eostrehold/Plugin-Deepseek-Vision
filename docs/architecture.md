# 架构边界

## 目标数据流

```text
CLIProxyAPI request
  -> host alias/model resolution
  -> request.intercept_after (plugin)
     -> gate SourceFormat/path/final Model
     -> select the Responses, Chat, or Claude downstream adapter
     -> group protocol-native image blocks by message/tool-result item
     -> one multi-image host.model.execute call per uncached prompt group,
        trying vision_model then ordered vision_fallback_models
     -> if enabled, correlate declared view_image/reanalyze calls with rich
        tool-output image blocks (tail call IDs <= 3)
     -> replace image positions with markers and append one joint analysis
  -> DeepSeek executor (only after all replacements succeed)
```

插件只做请求预处理，不重写响应流、不实现独立 OCR、不提供模型注册、executor、
HTTP 客户端或 CLIProxyAPI server-side tool。`host.model.execute` 负责每个候选模型的路由、
凭证、供应商协议转换、传输与重试；宿主会在嵌套请求中跳过当前插件，避免递归。配置快照在
单次请求期间保持一致。

正常图片组使用代际 LRU；重分析 `refresh` 由独立的、有界 call-ID 幂等缓存隔离，新的 call ID 只运行一次，
相同身份（call ID、解码/规范化图片指纹、focus、语言、完整模型链）重放可复用结果，`no_store` 不落入
跨请求缓存。`analysis_cache_size: 0` 只关闭普通 LRU。插件只把 tool output 中实际存在的图片块交给
分析器；arguments 的 attachment IDs 是 Agent-owned opaque handles，插件只校验，绝不解析或读取。

## 模块职责边界

- 契约、fixture、配置示例、文档和验证脚本：定义对外接口和验证入口。
- ABI、配置与插件生命周期：`main.go`、`rpc.go`、`version.go`、配置实现。
- 下游 Responses、Chat、Claude 请求发现与原协议改写：`internal/downstream/**`。
- 宿主模型适配与安全限制：`internal/vision/**`、`internal/safety/**`。
- 拦截集成：`internal/interceptor/**` 和集成脚本。

## SDK 依赖可复现性

`go.mod` 固定 `github.com/router-for-me/CLIProxyAPI/v7 v7.2.119`，与 CLIProxyAPI v7.2.119 的 SDK/API 版本一致。构建使用该版本模块，不写入绝对路径 `replace`。需要本地联调时，可将 `docs/examples/go.work.example` 复制到仓库根目录并命名为未跟踪的 `go.work`，再将工作区中的 SDK checkout 固定在 v7.2.119；`go.work` 和 `go.work.sum` 已被忽略，避免把本地工作区引用带入仓库。若 SDK 版本改变，应同步更新 `require`、`go.sum` 和本文件；完成依赖更新后运行 `GOTOOLCHAIN=auto go mod download` 生成 `go.sum`。

## 资源与安全边界

- 全局在途视觉请求数使用有界队列；CLIProxyAPI 继续负责供应商级并发、限流和重试。
- 唯一图片数仅保留默认 256 的异常负载兜底；引用大小、请求体、VLM 响应体和输出字符数仍有硬上限。
- 多图请求只有在宿主明确返回 413 时才按顺序拆分，不依据供应商特定错误文本猜测。
- 插件不接触、存储或转发 API key；凭证与供应商并发策略完全由 CLIProxyAPI 管理。
- 插件只缓存派生分析文本和不可逆哈希键，不保存图片引用或原图；可配置容量与 TTL 的 LRU 随
  配置代际更新而清空，日志只记录脱敏后的错误类别。
- `vision_fallback_models` 最多三个且按配置顺序尝试；终止 502 只携带不透明 `error_id`、固定
  错误码和安全 attempts 字段，host executor 细节保持通用。
- 默认清理本地附件路径。仅在开关启用、请求声明 `view_image` 且路径严格符合
  `.codex/attachments/<id>/` 时才保留；任何更宽路径都被重写为脱敏标记。
