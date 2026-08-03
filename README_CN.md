# deepseek-vision

`deepseek-vision` 是 CLIProxyAPI v7 原生插件：当最终上游模型为
`deepseek-v4-flash`（代码和配置同时保留未来支持的
`deepseek-v4-pro`）时，它会在 `/v1/responses` 请求进入
DeepSeek executor 前，用一次 `gpt-5.6-luna`（可配置的 OpenAI-compatible
Responses VLM）分析每张图片，并把图片替换为一段文本分析。

当前可用且本版本实际验收的目标只有 `deepseek-v4-flash`。
`deepseek-v4-pro` 的 Responses 服务尚不可用，本版本不要求、不探测，也不把
它作为真实网关测试条件；待上游可用后再补测。

插件对符合边界的 Responses 图片请求采用严格失败模式：VLM 调用失败就直接返回
502，原始图片不会继续发送给 DeepSeek。插件不实现独立 executor 或响应流转换；
未支持的协议继续由宿主正常处理。其他模型、其他 API、`/v1/responses/compact` 和
没有图片的请求全部旁路。`stream: true` 可以使用，因为图片预处理发生在响应流开始之前。

支持边界是有意收窄的：只有 `SourceFormat == "openai-response"`、元数据中的
`request_path == "/v1/responses"`，并且宿主解析后的最终 `Model` 命中
`target_models` 才会拦截。Anthropic Messages 和 Chat Completions 的图片转换尚未
实现；这些源协议不属于本契约，继续由宿主的正常链路处理。

请求中当前可见的整个 `input[]` 都会扫描。历史对话中已经保留在请求里的图片
（包括 Codex/Luna 期间留下的图片）和当前轮图片会一起转换；只有全部分析成功后，
被替换图片块及其引用才会从发往 DeepSeek 的 body 中移除。`previous_response_id` 只是
一个标识符：它指向的服务端隐藏历史不会出现在本次回调中，插件无法读取或改写，
不能据此保证隐藏历史图片已被转换。

对符合边界且含图片的 Responses 请求，JSON 结构错误返回 400，不支持的图片引用
返回 422，超过配置限制返回 413，VLM、超时或改写失败返回 502；失败会终止请求，
绝不会退化为转发原图。非目标协议和非目标模型保持旁路。

如果 runtime 在正常解析前不可用，目标模型的 Responses 请求遇到格式错误或疑似图片
结构时会保守地返回 502；非目标模型仍然旁路。这是生命周期保护，不改变正常运行时的
400/422/413 状态契约。

## 安装

Linux amd64 主机需要 Go 1.26、CGO 和 C 编译器：

```bash
VERSION=0.1.0 ./scripts/package.sh
./scripts/checksum.sh
plugin_dir=plugins/linux/amd64
mkdir -p "$plugin_dir"
find "$plugin_dir" -maxdepth 1 -type f -name 'deepseek-vision-v*.so' -delete
rm -f "$plugin_dir/deepseek-vision.so" "$plugin_dir/checksums.txt"
unzip -o dist/deepseek-vision_0.1.0_linux_amd64.zip -d "$plugin_dir"
test "$(find "$plugin_dir" -maxdepth 1 -type f -name 'deepseek-vision*.so' | wc -l)" -eq 1
(cd "$plugin_dir" && sha256sum -c checksums.txt)
```

这是手动模式：插件根目录通过 CLIProxyAPI 的 `CLI_PROXY_PLUGIN_PATH` 挂载，最终
只能有一个 `plugins/linux/amd64/deepseek-vision.so`，不能残留
`deepseek-vision-v*.so`。Store 管理模式则使用
`deepseek-vision-vX.Y.Z.so`，并在 `plugins.configs.deepseek-vision.store.version`
中固定相同版本；两种模式的升级、回滚和活动路径校验见
[`docs/installation.md`](docs/installation.md)。没有 `zip` 命令时，打包脚本自动
使用 Python `zipfile`。

复制 [`config.example.yaml`](config.example.yaml) 到 CLIProxyAPI 配置路径。仅在运行时
环境中设置配置的 VLM 凭据环境变量；不要提交凭据。

VLM 地址必须在配置中显式填写（插件不再内置可联网默认地址），默认模型为
`gpt-5.6-luna`。Docker Compose 示例见
[`docker/docker-compose.example.yml`](docker/docker-compose.example.yml)。
Docker 构建和安装、升级、回滚步骤见 [`docs/installation.md`](docs/installation.md)。

## 配置与安全

完整字段和硬限制见 [`docs/configuration.md`](docs/configuration.md)。API key 不写
入 YAML、镜像、ZIP、日志或 GitHub Actions；脚本会拒绝明显的凭据标记。VLM 请求
使用插件自有 HTTP client、超时、响应体限制和有限重试。安全边界见
[`docs/security.md`](docs/security.md)。

## 开发验证

```bash
go test ./...
go test -race ./...
go vet ./...
./scripts/verify-contracts.sh
./scripts/package-smoke.sh
```

契约和生命周期见 [`docs/contracts.md`](docs/contracts.md)，架构说明见
[`docs/architecture.md`](docs/architecture.md)，故障排查见
[`docs/troubleshooting.md`](docs/troubleshooting.md)。

## 许可证

本项目采用 MIT 许可证，详见 [`LICENSE`](LICENSE)。
