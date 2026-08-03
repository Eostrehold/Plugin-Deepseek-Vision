# deepseek-vision

> 🌐 文档语言：**简体中文** ｜ [English](README_EN.md)

为 DeepSeek Responses 补上图片理解预处理：先由视觉模型读图，再把视觉分析交给 DeepSeek 继续回答。

`deepseek-vision` 是一个 **CLIProxyAPI v7 插件**，不是独立代理服务，也不需要修改 CLIProxyAPI 源码。它让原本不接受图片的 DeepSeek Responses 模型可以处理带图对话：

1. 插件识别符合条件的 OpenAI Responses 图片请求；
2. 调用你配置的 OpenAI-compatible VLM 分析图片；
3. 将图片替换为包含可见文字和视觉描述的文本；
4. 再由 CLIProxyAPI 按原有路由把请求交给目标 DeepSeek 模型。

默认 VLM 模型为 `gpt-5.6-luna`，默认目标模型为 `deepseek-v4-flash`。VLM 地址必须由部署者显式配置。

## 适合哪些场景

- 把截图、报错界面、代码截图交给 DeepSeek 分析；
- 让 DeepSeek 阅读图片中的文字、表格、图表和界面布局；
- 在 OpenAI Responses 多轮请求中继续处理当前请求 body 仍然可见的历史图片；
- 为已有 CLIProxyAPI 部署增加图片预处理，而不单独维护另一套代理服务。

插件提供的是“图片转视觉分析文本”的预处理能力，不会把 DeepSeek 变成原生多模态模型。最终效果仍取决于所选 VLM 的识别质量。

## 当前支持范围

只有同时满足以下条件的请求才会进入图片转换：

| 条件 | 要求 |
| --- | --- |
| 请求协议 | OpenAI Responses，`SourceFormat=openai-response` |
| 请求路径 | 精确为 `/v1/responses` |
| 目标模型 | CLIProxyAPI 完成别名和模型池解析后的最终模型命中 `target_models` |
| 默认目标 | 仅 `deepseek-v4-flash` |

以下请求不会由本插件转换图片，而是继续交给 CLIProxyAPI 的正常链路处理：

- Chat Completions；
- Anthropic Messages；
- `/v1/responses/compact`；
- 非目标模型；
- 没有图片的请求；
- 其他协议或 API。

`deepseek-v4-pro` 不在默认目标列表中，也未纳入当前真实网关验收。只有在你的上游 Responses 服务已经可用并完成自行验证后，才应将它显式加入 `target_models`。

## 对话历史边界

插件会扫描本次请求 body 中当前可见的整个 `input[]`：

- 当前轮图片会被转换；
- 仍然保留在 body 中的历史轮次图片也会被转换；
- `previous_response_id` 背后的服务端隐藏历史不会展开给插件，因此无法读取或改写其中的图片。

如果业务依赖历史图片，请确保图片仍然出现在当前请求可见的 `input[]` 中。

## 安装前提

- CLIProxyAPI `v7.2.113`；
- Linux amd64；
- 支持 CGO 原生插件的 CLIProxyAPI 运行环境；
- 一个支持 OpenAI-compatible Responses API 的 VLM 服务；
- 可用的 VLM endpoint、模型名和 API key。

从源码构建还需要：

- Go 1.26；
- CGO 和 C 编译器；
- `python3`、`nm`、`strings`、`sha256sum`。

安装对象是 CLIProxyAPI 插件动态库，不需要修改或重新维护 CLIProxyAPI 源码。

## 安装

### 方式一：从源码构建

```bash
git clone https://github.com/Zesuy/Plugin-Deepseek-Vision.git
cd Plugin-Deepseek-Vision

VERSION=0.1.0 ./scripts/package.sh
./scripts/checksum.sh
```

构建结果位于：

```text
dist/deepseek-vision_0.1.0_linux_amd64.zip
dist/checksums.txt
```

先校验发布包：

```bash
cd dist
sha256sum -c checksums.txt
cd ..
```

然后解压并校验插件本体：

```bash
plugin_root=/path/to/plugins
tmp_dir="$(mktemp -d)"

unzip -q dist/deepseek-vision_0.1.0_linux_amd64.zip -d "$tmp_dir"
(cd "$tmp_dir" && sha256sum -c checksums.txt)

mkdir -p "$plugin_root/linux/amd64"
install -m 0755 "$tmp_dir/deepseek-vision.so" \
  "$plugin_root/linux/amd64/deepseek-vision.so"

rm -r "$tmp_dir"
```

手动安装模式下，活动文件应为：

```text
<CLI_PROXY_PLUGIN_PATH>/linux/amd64/deepseek-vision.so
```

重启前请确认该目录中只有一个 `deepseek-vision` 插件候选，不要同时保留未版本化的 `deepseek-vision.so` 和 `deepseek-vision-v*.so`：

```bash
find "$plugin_root/linux/amd64" \
  -maxdepth 1 -type f -name 'deepseek-vision*.so' -print
```

替换动态库后需要重启 CLIProxyAPI。

### 方式二：由 CLIProxyAPI 插件存储管理

如果你的 CLIProxyAPI 插件源已经收录本插件，可以通过宿主插件存储安装，并固定明确版本。存储管理模式使用类似下面的配置：

```yaml
plugins:
  enabled: true
  configs:
    deepseek-vision:
      enabled: true
      priority: 100
      store:
        source: <已配置的插件源>
        version: "0.1.0"
```

存储管理模式使用 `deepseek-vision-vX.Y.Z.so`，配置中的 `store.version` 必须与实际文件版本一致。不要与手动安装的未版本化文件混用。

详细升级、回滚和活动路径检查见 [安装文档](docs/installation.md)。

## 最小配置

将以下内容合并到 CLIProxyAPI 配置：

```yaml
plugins:
  enabled: true
  configs:
    deepseek-vision:
      enabled: true
      priority: 100

      target_models:
        - deepseek-v4-flash

      # 填 API base，例如 .../v1；不要在末尾再加 /responses
      vision_base_url: https://vlm.example.com/v1
      vision_model: gpt-5.6-luna

      # 这里填写环境变量名，不填写 API key
      vision_api_key_env: DEEPSEEK_VISION_API_KEY

      language: zh
```

插件会在 `vision_base_url` 后追加 `/responses`。该地址没有可联网的默认值，必须显式填写。

把 API key 注入 CLIProxyAPI 进程的运行时环境：

```text
DEEPSEEK_VISION_API_KEY=<your-vlm-api-key>
```

推荐使用 systemd、容器 secret 或部署平台的密钥管理能力。不要把 API key 写入 YAML、镜像、启动参数、仓库或日志。

完整配置和默认限制见 [配置文档](docs/configuration.md)。

### 通过宿主 management 配置

基础插件配置可通过 CLIProxyAPI 的 management 配置能力完成。插件注册信息提供 `ConfigFields` 元数据，具体管理界面和呈现方式由宿主决定。

本项目不提供或承诺独立的自定义 HTML 配置页面。

## 验证安装

### 1. 确认插件已注册并启用

```bash
curl -fsS \
  -H 'Authorization: Bearer <management-key>' \
  http://127.0.0.1:<management-port>/v0/management/plugins \
  | jq '.plugins[]
      | select(.id == "deepseek-vision")
      | {path, registered, effective_enabled, metadata}'
```

应确认：

- `registered` 为 `true`；
- `effective_enabled` 为 `true`；
- `path` 指向预期的 Linux amd64 插件文件；
- `metadata.version` 与安装版本一致。

### 2. 发送一条带图 Responses 请求

```bash
curl -sS \
  -H 'Authorization: Bearer <client-api-key>' \
  -H 'Content-Type: application/json' \
  -X POST http://127.0.0.1:<api-port>/v1/responses \
  --data-binary '{
    "model": "deepseek-v4-flash",
    "input": [{
      "role": "user",
      "content": [
        {
          "type": "input_text",
          "text": "请说明这张图片中的可见文字和主要内容。"
        },
        {
          "type": "input_image",
          "image_url": "<可由 VLM 访问的 HTTPS 图片 URL 或 data URI>"
        }
      ]
    }],
    "stream": false
  }'
```

成功时应观察到：

- 配置的 VLM 收到一次图片分析请求；
- DeepSeek 返回基于图片内容的回答；
- 发给目标 DeepSeek 的请求中，原始图片块已被视觉分析文本替换。

`stream: true` 也可使用，但图片预处理必须先完成，因此会增加首字节等待时间。

### 3. 可选：运行宿主端到端验收

仓库提供使用 mock VLM 和 mock 上游的宿主验收脚本，不需要真实 API key：

```bash
CLIPROXY_ROOT=/path/to/CLIProxyAPI ./scripts/host-e2e.sh
```

该验收覆盖插件注册、直接模型与别名、流式请求、多图改写、旁路规则、`file_id` 拒绝，以及 VLM 失败时返回 502 且不调用目标上游。

更多验证说明见 [测试文档](docs/testing.md)。

## 失败行为

对已经命中支持边界并包含图片的请求，插件采用 fail-closed 策略：

- JSON 结构错误通常返回 400；
- 不支持的图片引用（例如只有 `file_id`）返回 422；
- 超过请求体、图片数量或引用大小限制返回 413；
- VLM 请求失败、超时、结果无效或改写失败通常返回 502。

发生这些错误时，请求会被终止，不会退化为把原始图片发送给目标 DeepSeek。

这项保证只适用于命中插件支持边界的 OpenAI Responses 请求。其他协议和模型由 CLIProxyAPI 正常处理，本插件不承诺为它们移除图片。

## 图片格式限制

当前支持 Responses `input_image` 中的：

- HTTP/HTTPS `image_url`；
- data URI。

当前不支持：

- 仅提供 `file_id` 的图片；
- 需要额外文件下载 API 才能取得内容的引用；
- Chat Completions 或 Anthropic Messages 中的图片转换。

部分私网、回环或链路本地图片 URL 会因安全策略被拒绝。生产环境还应为 VLM 和图片访问配置合理的网络出口与 allowlist。

## 隐私、延迟与费用

使用本插件意味着图片会先交给你配置的 VLM 服务处理。部署前应确认该服务的数据保留、访问控制和数据驻留政策。

数据流边界如下：

- VLM 会收到图片引用及有限的上下文提示；
- 目标 DeepSeek 收到的是 VLM 生成的视觉分析文本，而不是原始图片；
- 插件的进程内缓存保存视觉分析文本和元数据，不保存原始图片字节；
- 缓存仅在当前进程内有效，重启或重新配置后不会保留。

性能与费用方面：

- 每张图片通常产生一次 VLM 调用；
- 多图请求会受配置的并发数和超时限制；
- VLM 预处理时间会增加请求延迟和流式响应首字节时间；
- 视觉分析文本会增加发送给 DeepSeek 的输入 token；
- 费用同时取决于 VLM 调用和目标模型 token 计费；
- 进程内缓存可能减少重复图片分析，但不应作为费用或命中率保证。

## 常见问题

### 这是一个新的 DeepSeek 代理服务吗？

不是。它是 CLIProxyAPI v7 的原生插件，依赖宿主完成鉴权、模型解析、路由和上游调用。

### 为什么我的图片请求没有被转换？

请依次确认：

1. 使用的是 `/v1/responses`；
2. 源协议是 OpenAI Responses；
3. CLIProxyAPI 解析后的最终模型命中 `target_models`；
4. 图片位于当前请求可见的 `input[]` 中；
5. 插件已注册并处于有效启用状态。

只修改请求中的别名并不能保证命中；插件以宿主解析后的最终模型为准。

### 支持 Chat Completions 或 Anthropic 图片吗？

暂不支持。这些协议由 CLIProxyAPI 按原有逻辑处理，本插件不会转换或移除其中的图片。

### 能处理 `previous_response_id` 中的历史图片吗？

不能。插件只能处理当前请求 body 中可见的图片。`previous_response_id` 指向的服务端隐藏历史不会提供给插件。

### 支持 `file_id` 吗？

暂不支持。只有 `file_id`、没有可用 `image_url` 的目标请求会被拒绝，而不会把未知图片引用继续发送给 DeepSeek。

### 为什么图片请求返回 502？

常见原因包括：

- VLM API key 没有注入 CLIProxyAPI 运行环境；
- `vision_base_url` 无法访问或错误地包含了 `/responses`；
- VLM 返回 401、403、5xx、超时或非法结果；
- VLM 响应超过配置限制。

对目标图片请求，502 是有意的安全失败行为：插件不会绕过预处理并转发原图。

### 可以把 `deepseek-v4-pro` 加入目标列表吗？

可以显式加入配置，但它不是当前默认或已验收目标。请先确认你的 DeepSeek 上游确实提供可用的 Responses 路径，并在自己的部署中完成端到端验证。

```yaml
target_models:
  - deepseek-v4-flash
  - deepseek-v4-pro
```

### 插件自带 VLM 服务或默认 VLM 地址吗？

不自带。默认模型名是 `gpt-5.6-luna`，但 endpoint 必须显式配置，你也可以换成其他兼容 OpenAI Responses 的 VLM。

## 更多文档

- [安装与升级](docs/installation.md)
- [完整配置](docs/configuration.md)
- [限制说明](docs/limitations.md)
- [安全边界](docs/security.md)
- [故障排查](docs/troubleshooting.md)
- [测试与验收](docs/testing.md)

## License

本项目采用 [MIT License](LICENSE)。

---

Copyright (c) 2026 Zesuy
