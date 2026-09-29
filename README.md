# Cursor API Proxy

把 Cursor 文本对话接口转换为 OpenAI 兼容 HTTP 服务。一个 Go 二进制即可运行，
无需 CPA、c-shared 插件或其他网关。

从 `AsterGateway/cliproxy-plugins` 的 Cursor 插件提取；来源和版权见
[NOTICE.md](NOTICE.md) 与 [LICENSE](LICENSE)。本仓库公开提供代码，保留上游版权与许可限制；公开不代表改用新的开源许可证。

## 能力与边界

- `GET /v1/models`：使用配置的 Cursor API key 获取该账号可用的 canonical model ID，缓存 5 分钟；发现失败会报错，不使用硬编码模型列表或 alias。
- `POST /v1/chat/completions`：文本消息、SSE 流式输出、`reasoning_content` 和真实上游 token 用量。
- 支持 `system`、`developer`、`user`、`assistant` 消息，以及字符串或 `type: text` 内容块。
- `GET /healthz`：无需鉴权的进程存活检查，不代表 Cursor 账号或上游可用。
- 每次请求创建独立 Cursor 会话；历史消息按带角色标记的文本拼接，客户端需要在每次请求中传入完整历史。这不是 Cursor 原生会话的恢复接口。

请求字段只支持 `model`、`messages`、`stream`、`stream_options.include_usage` 和
`n: 1`。工具调用、图片/音频、Responses API、采样参数、`max_tokens`、结构化输出等
未实现字段返回 400，不会静默忽略。角色标记属于文本提示，不提供原生 system-role 隔离。
上游要求执行工具时会返回失败；服务不会执行 shell、读写业务文件或访问 MCP 工具。

Cursor 私有 Connect/protobuf 协议可能随上游更新；模型出现在公开账号目录中也不保证
该账号/地区能够通过私有 wire 接口执行。上游权限、配额和协议错误会作为失败返回。

## Linux 二进制启动

需要 Go 1.26+。日常构建使用已提交的生成代码，只依赖 Go 和 protobuf runtime；
无需 `protoc`、C 编译器或 CPA SDK。

```bash
git clone https://github.com/tonycoder-hub/cursor-api-proxy.git
cd cursor-api-proxy
make build
```

通过已授权的密钥管理方式准备两个不同的密钥：Cursor API key 与调用本反代的
proxy API key。环境变量和 `*_FILE` 二选一；不要把密钥写进命令参数或提交到 Git。

```bash
export CURSOR_API_KEY_FILE=/path/to/cursor-api-key
export PROXY_API_KEY_FILE=/path/to/proxy-api-key
./bin/cursor-api-proxy
```

默认监听 `127.0.0.1:8787`。`-version` 可查看构建版本。SIGINT/SIGTERM 会取消正在
执行的上游请求并关闭服务。若对其他机器开放访问，应在前面配置带 TLS 的反向代理。

## Docker Compose

通过已授权的密钥管理方式准备 `.secrets/cursor_api_key` 与
`.secrets/proxy_api_key` 两个单行文件。目录权限设为 0700，文件设为 0600。
下面的启动命令让容器使用当前非 root 用户的 UID/GID，以便读取挂载的密钥文件；
不要用 root 用户执行这组命令。`.secrets/`、`.env`、运行数据和日志均已加入
Git 与 Docker context 忽略规则。

```bash
cp .env.example .env
RUN_AS_UID="$(id -u)" RUN_AS_GID="$(id -g)" docker compose up -d --build
curl --fail http://127.0.0.1:8787/healthz
```

镜像默认以 UID 65532 运行，Compose 示例使用当前非 root 用户身份。根文件系统只读，
默认只把端口映射到宿主机 loopback。
这些命令应在有相应构建与部署权限的 Linux 环境执行。
如构建环境无法访问默认 Go 模块代理，可通过 Docker 构建参数 `GOPROXY` 指定该环境
已配置的可用模块代理；依赖校验仍使用提交的 `go.sum`。

## 调用方式

用已经装有 OpenAI Python SDK 的客户端环境调用：

```python
import os
from pathlib import Path
from openai import OpenAI

client = OpenAI(
    base_url="http://127.0.0.1:8787/v1",
    api_key=Path(os.environ["PROXY_API_KEY_FILE"]).read_text().strip(),
)
model = client.models.list().data[0].id
response = client.chat.completions.create(
    model=model,
    messages=[{"role": "user", "content": "你好，简短介绍一下自己"}],
)
print(response.choices[0].message.content)

for chunk in client.chat.completions.create(
    model=model,
    messages=[{"role": "user", "content": "写一句简短的问候"}],
    stream=True,
    stream_options={"include_usage": True},
):
    if chunk.choices:
        print(chunk.choices[0].delta.content or "", end="", flush=True)
```

任意 HTTP 客户端使用 `Authorization: Bearer <proxy-api-key>`；Cursor API key
仅由服务端使用，不传给调用方。不要直接使用模型 alias（例如 `auto`），先查 `/v1/models`。

## 配置

| 变量 | 默认值 / 要求 |
| --- | --- |
| `CURSOR_API_KEY` / `CURSOR_API_KEY_FILE` | 必填，Cursor API key，二选一 |
| `PROXY_API_KEY` / `PROXY_API_KEY_FILE` | 必填，独立的下游访问密钥，二选一 |
| `LISTEN_ADDR` | `127.0.0.1:8787` |
| `REQUEST_TIMEOUT` | `5m`，允许 `1s`–`30m` |
| `MAX_CONCURRENT_REQUESTS` | `4`，允许 `1`–`64`；超过限制返回 429 |
| `CURSOR_RPC_BASE_URL` | `https://api2.cursor.sh`，仅用于受信任的部署配置或测试 |
| `CURSOR_MODELS_URL` | `https://api.cursor.com/v1/models`，仅用于受信任的部署配置或测试 |

上游 URL 只允许 HTTPS，或用于测试的 loopback HTTP；禁止 URL 内嵌凭据、query 和
fragment。上游重定向不会被跟随。修改这两个地址会改变凭据的接收方，应仅指向可信服务。
请求体上限 1 MiB，输出累计上限 8 MiB，单个 Connect frame / 解压内容上限 16 MiB。
同一进程只配置一个 Cursor 账号；不同账号请使用独立实例和访问密钥。

## 错误行为

| 状态 | 含义 |
| --- | --- |
| 400 | 请求无效、字段不支持或模型不在当前账号目录中 |
| 401 | 缺少或错误的反代访问密钥 |
| 413 / 415 | 请求体过大 / Content-Type 不是 application/json |
| 429 | 并发请求达到上限 |
| 502 / 504 | 上游失败或请求超时 |

流式响应在首条内容前失败会返回 HTTP 错误；已开始输出后失败会发送包含 `error` 的
SSE 事件并关闭，不会再发送 `finish_reason: stop` 或 `[DONE]`。只有上游明确发送
turn-ended 且最终 Connect trailer 成功，才会结束为成功。错误不会回显上游原始正文或凭据。

## 开发与验证

```bash
make fmt-check
make test
make vet
make build
```

测试使用本地模拟的 Cursor HTTP/protobuf 上游，不需要真实密钥，也不会访问真实账号。
覆盖原有协议回归测试、鉴权、模型发现、完整 wire 握手、blob 往返、SSE 错误、请求取消、
并发限制和内存边界。模拟测试不能证明当前线上 Cursor 协议与账号权限仍然可用。

更新协议后才需要重新生成：

```bash
make generate PROTOC_GEN_GO=/path/to/protoc-gen-go
```

本次生成使用 `protoc 34.0` 与 `protoc-gen-go v1.36.10`；运行时版本由 `go.mod` 固定。
字段编号必须保持与上游协议一致。
`ci/github-actions.yml.example` 提供手动 GitHub Actions 模板，默认未启用。
维护者具备 workflow 写入权限并配置好标签为 `cursor-proxy` 的专用 Linux self-hosted
runner 后，可将模板复制到 `.github/workflows/ci.yml`。模板仅支持手动触发，
push 与外部 PR 不会触发该 runner 上的构建。
项目维护规则见 [CONTRIBUTING.md](CONTRIBUTING.md)。
