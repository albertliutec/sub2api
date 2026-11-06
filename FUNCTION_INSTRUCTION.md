# 功能与开发说明

本文档汇总项目的本地开发流程与 Trae 上游能力边界，作为开发、联调、对接的统一参考。

- 适用代码：`backend/`、`frontend/`
- 上游协议：`POST /api/ide/v2/llm_raw_chat`（Trae 私有 SSE）
- 文档更新：2026-11-06

---

# 第一部分：项目与本地开发

## 一、项目基本信息

| 项目 | 说明 |
|------|------|
| **上游仓库** | Wei-Shaw/sub2api |
| **Fork 仓库** | bayma888/sub2api-bmai |
| **技术栈** | Go 后端（Ent ORM + Gin）+ Vue3 前端（pnpm） |
| **数据库** | PostgreSQL 16 + Redis |
| **包管理** | 后端：go modules；前端：**pnpm**（不是 npm） |

## 二、本地环境配置

### PostgreSQL 16（Windows 服务）

| 配置项 | 值 |
|--------|-----|
| 端口 | 5432 |
| psql 路径 | `C:\Program Files\PostgreSQL\16\bin\psql.exe` |
| pg_hba.conf | `C:\Program Files\PostgreSQL\16\data\pg_hba.conf` |
| 数据库凭据 | user=`sub2api`, password=`sub2api`, dbname=`sub2api` |
| 超级用户 | user=`postgres`, password=`postgres` |

### Redis

| 配置项 | 值 |
|--------|-----|
| 端口 | 6379 |
| 密码 | 无 |

### 开发工具

```bash
# golangci-lint（CI 用 v2.13，本地建议装同一版以免版本差异带来的噪音）
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13

# pnpm（前端包管理，固定 10.x，pnpm 12+ 不兼容，见坑 12）
npm install -g pnpm@10
```

## 三、本地 dev 环境启动

以下流程已在 Windows + 本地 `backend/config.yaml` 指向远程 PostgreSQL/Redis 的环境下实测跑通。

### 1. 前置依赖

| 依赖 | 要求 | 说明 |
|------|------|------|
| Go | `go.mod` 要求 1.27.3 | 本地只装 1.25.x 也可，`GOTOOLCHAIN=auto`（默认）会自动下载并使用 1.27.3 |
| Node.js | 24.x | |
| pnpm | **10.x** | 不要用 pnpm 12+，见坑 12 |
| 数据库 | PostgreSQL 16 / Redis | 本地实例或远程实例均可，配置写在 `backend/config.yaml` |

### 2. 后端配置（backend/config.yaml）

配置文件按以下顺序查找（见 `backend/internal/config/config.go` 的 `configureConfigSource`）：

1. 环境变量 `CONFIG_FILE` 指定的绝对路径（最高优先级）
2. `$DATA_DIR/config.yaml`
3. `/app/data/config.yaml`
4. **当前工作目录下的 `config.yaml`**（本地开发用这个）
5. `./config/config.yaml`
6. `/etc/sub2api/config.yaml`

> 因此后端**必须在 `backend` 目录下启动**，否则读不到 `backend/config.yaml`。`NeedsSetup()` 也以该文件是否存在判断是否需要进入安装向导，文件存在就不会弹 setup。

配置项支持环境变量覆盖，规则是 key 中的 `.` 替换为 `_` 并大写，例如 `DATABASE_HOST`、`DATABASE_PASSWORD`、`REDIS_PASSWORD`、`SERVER_PORT`。

本地 dev 建议在 `backend/config.yaml` 里加上（该文件已被 `.gitignore` 忽略，改动只影响本地）：

```yaml
server:
    mode: debug          # gin debug 模式，便于调试
log:
    level: debug
    output:
        to_stdout: true
        to_file: false   # Windows 下默认日志路径是容器路径 /app/data/logs，会报 Access denied
```

### 3. 启动后端

```powershell
cd backend

# 方式一：直接跑（首次会自动下载依赖并编译，耗时较长）
go run ./cmd/server/

# 方式二：先编译再运行
go build -o bin/server-dev.exe ./cmd/server
.\bin\server-dev.exe
```

启动成功的关键日志：`Server started on 0.0.0.0:8080`。

验证：

```powershell
curl http://127.0.0.1:8080/health                       # 200
curl http://127.0.0.1:8080/api/v1/settings/public      # 200，返回站点公开配置
```

> 本地开发不需要加 `-tags embed`。`embed` 仅用于单二进制部署（把前端产物打进后端二进制），本地用独立的 Vite dev server 才能保留热更新。

### 4. 启动前端

```powershell
cd frontend

npx pnpm@10 install   # 首次或 node_modules 缺失时执行（不要用 pnpm 12）
npx pnpm@10 dev       # 启动 Vite dev server
```

- 默认端口 3000；被占用时 Vite 会自动顺延到 3001，并在日志里打印实际地址。
- `/api`、`/v1`、`/setup` 会被代理到 `VITE_DEV_PROXY_TARGET`（默认 `http://localhost:8080`）。
- 需要改端口/代理目标时，在 `frontend/.env.development` 中设置：

```dotenv
VITE_DEV_PORT=3000
VITE_DEV_PROXY_TARGET=http://localhost:8080
```

验证：浏览器打开 `http://localhost:3000`（或实际端口），页面能正常加载，且 HTML 中已注入 `window.__APP_CONFIG__`（由 Vite 插件 `injectPublicSettings` 从后端 `/api/v1/settings/public` 拉取）。

### 5. 前后端联调链路

```
浏览器 :3000 (Vite dev)
  ├─ /api、/v1、/setup ──proxy──> 后端 :8080
  └─ 其余静态资源由 Vite 直接提供（热更新）
```

- 改前端代码：浏览器热更新，无需重启。
- 改后端代码：重启 `go run ./cmd/server/`（或重新编译）。
- 改 `ent/schema/*.go`：先 `go generate ./ent` 再重启。

---

# 第二部分：Trae 上游能力支持矩阵

## 四、结论摘要

| 分类 | 数量 | 说明 |
|---|---|---|
| 完整支持 | 15 项 | 可无损转发 |
| 降级支持 | 4 项 | 能跑通但语义有损 |
| 不支持 | 14 项 | 上游协议本身没有对应字段 |

**核心结论：Trae 不是标准 OpenAI 接口，而是私有代理协议。** 请求体仅有 `messages` 与 `tools` 两个真正的自由度，采样类参数无法下发。

## 五、上游协议事实

调用链由三段组成，均使用 IDE 客户端指纹头（`X-App-Id`、`X-Ide-Version`、`X-Ide-Function` 等）：

| 环节 | 端点 | 说明 |
|---|---|---|
| 认证 | `POST /cloudide/api/v3/trae/oauth/ExchangeToken` | 用 PAT 换取 `Cloud-IDE-JWT` |
| 模型表 | `POST /api/ide/v1/cli/get_config_list` | 返回 `config_name` → `model_name`，带 `__v2` 等灰度后缀且会动态变化 |
| 对话 | `POST /api/ide/v2/llm_raw_chat` | 私有 SSE 流，唯一的推理入口 |

### 5.1 `llm_raw_chat` 接受的字段

从 Trae IDE 客户端 `@byted-icube/ai-modules-chat` 的请求构造器确认，请求体字段为：

```
config_name, model_name, session_id, messages, message, conversation_id,
is_custom_model, provider, encrypted_model_params, user_input, request_type,
metadata, tools, queue_id, request_seq, mode_type, chat_mode, config_source,
access_type, image_config, estimate_prompt_tokens, slash_command,
message_origin, tool_choice, reasoning_effort, stream, retry_times,
queue_retry, video_config, connect_check_param, user_selected_model_name,
reasoning_effort_level, estimated_token_usage, custom_model_uniq_id,
provider_context, biz_context, extra_info, use_fast_request
```

**其中不包含**：`temperature`、`top_p`、`max_tokens`、`stop`、`n`、`presence_penalty`、`frequency_penalty`、`response_format`、`parallel_tool_calls`、`logprobs`、`seed`。这是协议层缺失，不是实现遗漏。

### 5.2 模型配置侧的固定项

客户端模型配置结构体包含 `native_function_call`、`parallel_tool_calling`、`use_v2_process`、`enable_invalid_json_hint` 等字段 —— **并行工具调用、原生函数调用等能力由上游模型配置决定，调用方无权下发**。

### 5.3 请求/响应消息形态

- 请求 `messages` 为 Anthropic 风格 block 数组：`{"role":"user","content":[{"type":"text","text":"..."}]}`
- `tools` 为 OpenAI 嵌套式，但 `parameters` 需序列化为 **JSON 字符串**
- 响应为自定义 SSE，事件包括 `metadata` / `output` / `token_usage` / `done` / `error`
- `output` 事件中的工具调用使用上游特有字段名 `function_call`，参数可能是分片（`partial_arguments`）

## 六、完整支持的能力

### 6.1 入站协议

| 能力 | Chat Completions | Anthropic Messages | Responses API |
|---|---|---|---|
| 流式（SSE） | ✅ | ✅ | ✅ |
| 非流式 | ✅ | ✅ | ✅ |
| 协议自动识别 | ✅ | ✅ | ✅ |
| 流式增量经桥接层实时转换 | ✅ | ✅ | ✅ |

### 6.2 请求侧

| 能力 | 状态 | 说明 |
|---|---|---|
| 多轮对话历史 | ✅ | `user` / `assistant` / `system` 角色透传 |
| 系统提示词 | ✅ | OpenAI `instructions` / Anthropic `system` / Responses `instructions` 统一转为首条 `system` 消息 |
| Function tools 定义透传 | ✅ | `tools[].function.{name,description,parameters}` 原样透传 |
| `tool_choice` 透传 | ✅ | 支持 `"auto"` / `"required"` / `{"type":"function",...}`；`"none"` 或 `{"type":"none"}` 时清空 tools |
| `reasoning_effort` 透传 | ✅ | 三种入站协议的推理强度统一映射到上游该字段 |
| `stream` 显式下发 | ✅ | 与上游实际调用模式一致 |
| 图片输入 | ✅ | URL 自动下载转 data URL；已是 data URL 直接透传；上限 10MB |
| 多模态模型校验 | ✅ | 非多模态模型收到图片时返回 400，避免上游静默失败 |
| 模型名解析 | ✅ | 走 `get_config_list` 注册表，兼容 `__v2` 等灰度后缀 |
| 模型列表缓存 | ✅ | 5 分钟缓存 + 内置离线兜底列表 |

### 6.3 响应侧

| 能力 | 状态 | 说明 |
|---|---|---|
| 文本增量输出 | ✅ | 流式逐 token 下发 |
| 推理过程输出 | ✅ | `reasoning_content` 独立字段下发（三种协议各自还原） |
| 工具调用解析 | ✅ | 解析 SSE `output` 事件的 `tool_calls` |
| 多形态工具调用兼容 | ✅ | 同时兼容上游式 `function_call`、OpenAI 式 `function`、扁平式 `name/arguments`、Anthropic 式 `tool_use/input` |
| 工具调用增量聚合 | ✅ | 按 `index` 累积参数分片；完整 JSON 重发时执行覆盖而非追加，避免上游重试导致参数翻倍 |
| 下游工具调用还原 | ✅ | Chat Completions → `tool_calls`；Anthropic → `tool_use` block；Responses → `function_call` item |
| `finish_reason` 映射 | ✅ | `length` / `max_tokens` → `length`；`tool_calls` / `tool_use` → `tool_calls`；`content_filter` 保留；其余归一为 `stop`；有工具调用时兜底为 `tool_calls` |
| Usage 统计 | ✅ | prompt / completion / total / reasoning / cache_read 全量映射 |

### 6.4 链路可靠性

| 能力 | 状态 | 说明 |
|---|---|---|
| JWT 缓存 | ✅ | 内存 single-flight，提前 10 分钟过期 |
| 401 自动重换 token | ✅ | 仅在未向客户端推送数据时重试 |
| 瞬时错误重试 | ✅ | 未推送前重试一次 |
| 客户端断连中断 | ✅ | Context 取消透传，返回 499 |
| 账号代理 / 并发控制 | ✅ | 按账号配置走 HTTP 代理与并发限流 |
| 上游余额/额度探测 | ❌ | Trae 无公开余额查询接口，无法探测与自动暂停 |

## 七、降级支持（可用但有损）

| 能力 | 降级行为 | 影响 |
|---|---|---|
| 多轮工具调用历史 | assistant 的 `tool_calls` 被序列化为文本 `[tool calls] {...}`；`role:"tool"` 被改写为 `role:"user"`，**`tool_call_id` 丢失** | 单轮工具调用正常；多轮 agent 循环中模型无法精确关联工具结果与调用 |
| legacy `function_call` | 序列化为文本 `[function call] {...}` | 同上 |
| 历史 `reasoning_content` | 作为文本块拼入 assistant 消息 | 上下文变长，且可能与上游自身推理机制重复 |
| 文件 / PDF 输入 | 仅保留占位文本 `[attached file: <name>]` | 文件内容不会被模型读取 |

## 八、不支持的能力

### 8.1 采样与生成参数

| 参数 | 表现 |
|---|---|
| `temperature` | 静默忽略，按上游默认值 |
| `top_p` | 静默忽略 |
| `max_tokens` / `max_completion_tokens` | 静默忽略，长度由上游模型配置决定 |
| `stop` / `stop_sequences` | 静默忽略 |
| `seed` | 静默忽略 |
| `presence_penalty` / `frequency_penalty` | 静默忽略 |
| `logit_bias` | 静默忽略 |
| `n > 1` | 永远返回单候选 |
| `logprobs` / `top_logprobs` | 不支持，上游 SSE 无 token 概率 |

### 8.2 结构化输出

| 能力 | 表现 |
|---|---|
| `response_format`（`json_object` / `json_schema`） | 静默忽略，不强制 JSON 输出 |
| Responses API `text.format` | 静默忽略 |
| 严格模式 `strict` | 静默忽略 |

### 8.3 有状态会话

| 能力 | 原因 |
|---|---|
| `previous_response_id` | 上游每次请求随机生成 `conversation_id` / `session_id`，无服务端会话延续 |
| `store` | 无持久化存储语义，无法实现"只发增量、服务端保存历史" |

### 8.4 工具相关限制

| 能力 | 原因 |
|---|---|
| `parallel_tool_calls` | 由上游模型配置 `parallel_tool_calling` 决定，调用方无权控制 |
| 服务端内置工具：`web_search`、`file_search`、`code_interpreter`、`computer_use`、`image_generation`、`x_search`、`local_shell`、`custom`、`namespace` | Trae 无对应执行环境；最多摊平为普通 function 声明，上游不会真正执行 |

### 8.5 多模态限制

| 能力 | 状态 |
|---|---|
| 图片输入（JPEG/PNG/WebP 等） | ✅ 支持，≤10MB |
| 图片 > 10MB | ❌ 返回 400 |
| 非多模态模型 + 图片 | ❌ 返回 400 |
| 音频输入 | ❌ 不支持 |
| 视频输入 | ❌ 不支持 |
| PDF / 文档解析 | ❌ 仅占位文本 |
| 图片生成 / 图像输出 | ❌ 不支持 |

### 8.6 其他元数据

`metadata`、`user`、`service_tier`、`include`、`prompt_cache_key`、`prompt_cache_retention`、`safety_identifier`、`system_fingerprint` 均不支持或会被丢弃。

## 九、行为约定与错误码

| 场景 | 处理 |
|---|---|
| 请求无任何 messages | 400 `request contains no messages` |
| 图片 URL 为空 / 下载失败 / 非 2xx | 400 `invalid_request_error` |
| 图片超过 10MB | 400 `image exceeds the 10MB limit` |
| 非多模态模型收到图片 | 400 `model xxx does not support image input` |
| 上游模型不存在 | 404 `invalid_request_error` |
| 上游鉴权失败 | 502 `authentication_error`，并触发账号级 failover |
| 上游瞬时错误 | 透传上游状态码，触发账号级 failover |
| 客户端主动断开 | 499 `request_cancelled` |

---

# 第三部分：工程规范与运维

## 十、CI/CD 流水线

### GitHub Actions Workflows

| Workflow | 触发条件 | 检查内容 |
|----------|----------|----------|
| **backend-ci.yml** | push, pull_request | 单元测试 + 集成测试 + golangci-lint v2.13 |
| **security-scan.yml** | push, pull_request, 每周一 | govulncheck + gosec + pnpm audit |
| **release.yml** | tag `v*` | 构建发布（PR 不触发） |

### CI 要求

- Go 版本必须是 **1.27.0**：三个 workflow 都用 `go-version-file: backend/go.mod` 取版本，随后硬断言 `go version | grep -q 'go1.27.0'`。升级 Go 时要同时改 `backend/go.mod`、`backend-ci.yml`（两处）、`release.yml`、`security-scan.yml` 里的这句断言，**以及三个 Dockerfile 里的 Go 构建镜像**（`Dockerfile` / `deploy/Dockerfile` 的 `ARG GOLANG_IMAGE`、`backend/Dockerfile` 的 `FROM golang:`）。前者漏了 CI 会在版本校验步骤直接失败；**后者漏了 CI 不会报，而是等到有人用这些 Dockerfile 构建时才失败**（`go.mod requires go >= X (running Y; GOTOOLCHAIN=local)`）。
- 前端使用 `pnpm install --frozen-lockfile`，必须提交 `pnpm-lock.yaml`。

## 十一、测试与自检

```bash
cd backend

# 编译
go build ./...

# 后端单元测试
go test -tags=unit ./...

# 后端集成测试
go test -tags=integration ./...

# 代码质量检查
golangci-lint run ./...

# Trae 相关：tools 透传、增量聚合、重发覆盖语义
go test ./internal/pkg/trae/... -v

# 协议桥接层
go test ./internal/pkg/apicompat/...
```

关键测试用例：

| 用例 | 覆盖点 |
|---|---|
| `TestJWTCachesPATExchange` | JWT single-flight 缓存与失效重换 |
| `TestListModelsUsesLiveRegistryAndFiltersChatModels` | 模型注册表拉取与过滤 |
| `TestChatConsumesTraeSSE` | SSE 事件消费 |
| `TestChatForwardsToolsAndAggregatesToolCalls` | tools 透传格式、`tool_choice`/`reasoning_effort` 下发、分片聚合、重发覆盖 |

## 十二、常见坑点 & 解决方案

### 坑 1：pnpm-lock.yaml 必须同步提交

**问题**：`package.json` 新增依赖后，CI 的 `pnpm install --frozen-lockfile` 失败。

**原因**：上游 CI 使用 pnpm，lock 文件不同步会报错。

**解决**：
```bash
cd frontend
pnpm install  # 更新 pnpm-lock.yaml
git add pnpm-lock.yaml
git commit -m "chore: update pnpm-lock.yaml"
```

---

### 坑 2：npm 和 pnpm 的 node_modules 冲突

**问题**：之前用 npm 装过 `node_modules`，pnpm install 报 `EPERM` 错误。

**解决**：
```bash
cd frontend
rm -rf node_modules  # 或 PowerShell: Remove-Item -Recurse -Force node_modules
npx pnpm@10 install
```

---

### 坑 3：PowerShell 中 bcrypt hash 的 `$` 被转义

**问题**：bcrypt hash 格式如 `$2a$10$xxx...`，PowerShell 把 `$2a` 当变量解析，导致数据丢失。

**解决**：将 SQL 写入文件，用 `psql -f` 执行：
```bash
# 错误示范（PowerShell 会吃掉 $）
psql -c "INSERT INTO users ... VALUES ('$2a$10$...')"

# 正确做法
echo "INSERT INTO users ... VALUES ('\$2a\$10\$...')" > temp.sql
psql -U sub2api -h 127.0.0.1 -d sub2api -f temp.sql
```

---

### 坑 4：psql 不支持中文路径

**问题**：`psql -f "D:\中文路径\file.sql"` 报错找不到文件。

**解决**：复制到纯英文路径再执行：
```bash
cp "D:\中文路径\file.sql" "C:\temp.sql"
psql -f "C:\temp.sql"
```

---

### 坑 5：PostgreSQL 密码重置流程

**场景**：忘记 PostgreSQL 密码。

**步骤**：
1. 修改 `C:\Program Files\PostgreSQL\16\data\pg_hba.conf`
   ```
   # 将 scram-sha-256 改为 trust
   host    all    all    127.0.0.1/32    trust
   ```
2. 重启 PostgreSQL 服务
   ```powershell
   Restart-Service postgresql-x64-16
   ```
3. 无密码登录并重置
   ```bash
   psql -U postgres -h 127.0.0.1
   ALTER USER sub2api WITH PASSWORD 'sub2api';
   ALTER USER postgres WITH PASSWORD 'postgres';
   ```
4. 改回 `scram-sha-256` 并重启

---

### 坑 6：Go interface 新增方法后 test stub 必须补全

**问题**：给 interface 新增方法后，编译报错 `does not implement interface (missing method XXX)`。

**原因**：所有测试文件中实现该 interface 的 stub/mock 都必须补上新方法。

**解决**：
```bash
# 搜索所有实现该 interface 的 struct
cd backend
grep -r "type.*Stub.*struct" internal/
grep -r "type.*Mock.*struct" internal/

# 逐一补全新方法
```

---

### 坑 7：Windows 上 psql 连 localhost 的 IPv6 问题

**问题**：psql 连 `localhost` 先尝试 IPv6 (::1)，可能报错后再回退 IPv4。

**建议**：直接用 `127.0.0.1` 代替 `localhost`。

---

### 坑 8：Windows 没有 make 命令

**问题**：CI 里用 `make test-unit`，本地 Windows 没有 make。

**解决**：直接用 Makefile 里的原始命令：
```bash
# 代替 make test-unit
go test -tags=unit ./...

# 代替 make test-integration
go test -tags=integration ./...
```

---

### 坑 9：Ent Schema 修改后必须重新生成

**问题**：修改 `ent/schema/*.go` 后，代码不生效。

**解决**：
```bash
cd backend
go generate ./ent  # 重新生成 ent 代码（json.RawMessage 字段会生成为同类型的 jsontext.Value，属预期）
git add ent/       # 生成的文件也要提交
```

---

### 坑 10：前端测试看似正常，但后端调用失败（模型映射被批量误改）

**典型现象**：
- 前端按钮点测看起来正常；
- 实际通过 API/客户端调用时返回 `Service temporarily unavailable` 或提示无可用账号；
- 常见于 OpenAI 账号（例如 Codex 模型）在批量修改后突然不可用。

**根因**：
- OpenAI 账号编辑页默认不显式展示映射规则，容易让人误以为"没映射也没关系"；
- 但在**批量修改同时选中不同平台账号**（OpenAI + Antigravity/Gemini）时，模型白名单/映射可能被跨平台策略覆盖；
- 结果是 OpenAI 账号的关键模型映射丢失或被改坏，后端选不到可用账号。

**修复方案（按优先级）**：
1. **快速修复（推荐）**：在批量修改中补回正确的透传映射（例如 `gpt-5.3-codex -> gpt-5.3-codex-spark`）。
2. **彻底重建**：删除并重新添加全部相关账号（最稳但成本高）。

**关键经验**：
- 如果某模型已被软件内置默认映射覆盖，通常不需要额外再加透传；
- 但当上游模型更新快于本仓库默认映射时，**手动批量添加透传映射**是最简单、最低风险的临时兜底方案；
- 批量操作前尽量按平台分组，不要混选不同平台账号。

---

### 坑 11：PR 提交前检查清单

提交 PR 前务必本地验证：

- [ ] `go test -tags=unit ./...` 通过
- [ ] `go test -tags=integration ./...` 通过
- [ ] `golangci-lint run ./...` 无新增问题
- [ ] `pnpm-lock.yaml` 已同步（如果改了 package.json）
- [ ] 所有 test stub 补全新接口方法（如果改了 interface）
- [ ] Ent 生成的代码已提交（如果改了 schema）

---

### 坑 12：pnpm 12+ 无法启动前端

**问题**：

```
Error: ERR_PNPM_VERIFY_DEPS_BEFORE_RUN
Your node_modules are out of sync with your lockfile, and installing would
drop "pnpm.overrides" from the lockfile, because the "pnpm" field in
package.json is no longer read by pnpm
```

**原因**：pnpm 12 起不再读取 `package.json` 的 `pnpm.overrides`，要求把配置迁移到 `pnpm-workspace.yaml`；直接 `pnpm install` 会把 lockfile 里的 overrides 丢掉。本项目 `pnpm-lock.yaml` 是 `lockfileVersion: 9.0`，对应 pnpm 9/10。

**解决**：用 pnpm 10 执行，不改动仓库文件：

```powershell
npx pnpm@10 install
npx pnpm@10 dev
```

或把全局 pnpm 降到 10：`npm install -g pnpm@10`。

---

### 坑 13：Windows 下后端日志报 `write /app/data/logs/sub2api.log: Access is denied`

**原因**：未配置 `log.output.file_path` 时，默认回退到容器内路径 `/app/data/logs/sub2api.log`（见 `backend/internal/pkg/logger/options.go`），Windows 上该路径不可写。

**解决**：在 `backend/config.yaml` 中关闭文件日志：

```yaml
log:
    output:
        to_stdout: true
        to_file: false
```

## 十三、常用命令速查

### 数据库操作

```bash
# 连接数据库
psql -U sub2api -h 127.0.0.1 -d sub2api

# 查看所有用户
psql -U postgres -h 127.0.0.1 -c "\du"

# 查看所有数据库
psql -U postgres -h 127.0.0.1 -c "\l"

# 执行 SQL 文件
psql -U sub2api -h 127.0.0.1 -d sub2api -f migration.sql
```

### Git 操作

```bash
# 同步上游
git fetch upstream
git checkout main
git merge upstream/main
git push origin main

# 创建功能分支
git checkout -b feature/xxx

# Rebase 到最新 main
git fetch upstream
git rebase upstream/main
```

### 前端操作

```bash
# 安装依赖（必须用 pnpm 10.x）
cd frontend
npx pnpm@10 install

# 开发服务器
npx pnpm@10 dev

# 构建
npx pnpm@10 build
```

### 后端操作

```bash
# 运行服务器
cd backend
go run ./cmd/server/

# 生成 Ent 代码
go generate ./ent

# 运行测试
go test -tags=unit ./...
go test -tags=integration ./...

# Lint 检查
golangci-lint run ./...
```

## 十四、项目结构速览

```
sub2api-bmai/
├── backend/
│   ├── cmd/server/          # 主程序入口
│   ├── ent/                 # Ent ORM 生成代码
│   │   └── schema/          # 数据库 Schema 定义
│   ├── internal/
│   │   ├── handler/         # HTTP 处理器
│   │   ├── service/         # 业务逻辑
│   │   │   └── trae_gateway.go   # Trae 上游网关转发
│   │   ├── pkg/
│   │   │   ├── trae/        # Trae 私有协议客户端
│   │   │   └── apicompat/   # 三协议互转桥接层
│   │   ├── repository/      # 数据访问层
│   │   └── server/          # 服务器配置
│   ├── migrations/          # 数据库迁移脚本
│   └── config.yaml          # 配置文件
├── frontend/
│   ├── src/
│   │   ├── api/             # API 调用
│   │   ├── components/      # Vue 组件
│   │   ├── views/           # 页面视图
│   │   ├── types/           # TypeScript 类型
│   │   └── i18n/            # 国际化
│   ├── package.json         # 依赖配置
│   └── pnpm-lock.yaml       # pnpm 锁文件（必须提交）
└── FUNCTION_INSTRUCTION.md  # 本文档
```

## 十五、参考资源

- [上游仓库](https://github.com/Wei-Shaw/sub2api)
- [Ent 文档](https://entgo.io/docs/getting-started)
- [Vue3 文档](https://vuejs.org/)
- [pnpm 文档](https://pnpm.io/)
