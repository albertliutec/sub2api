# Trae 上游渠道

sub2api 通过 Trae HTTP 直连接口使用 Trae 订阅账号，不依赖本机 `traecli` 进程。

## 前置条件

- 一个可用的 Trae 账号。
- 在 Trae 个人设置/账号页生成 PAT（Personal Access Token）。
- sub2api 服务端能访问 `https://api.enterprise.trae.cn`。

## 创建账号

1. 进入管理端 `账号`，点击创建账号。
2. 平台选择 `Trae`。
3. `API Key` 填写 Trae PAT。该字段会作为凭据保存，不会出现在响应明文中。
4. 保持默认上游地址 `https://api.enterprise.trae.cn`，或按自建 Trae 网关修改。
5. 保存后执行一次“测试连接”，确认 PAT 能换取 JWT 并拉取模型注册表。
6. 点击“同步模型”。Trae 的 `config_name` 会随灰度变化，运行时也会在首次请求及注册表过期后自动刷新，因此不要长期依赖硬编码模型列表。

## 创建订阅渠道

1. 在 `分组` 中创建平台为 `Trae` 的分组，并把 Trae 账号绑定到该分组。
2. 在分组的模型列表中加入要开放的 Trae 模型（“同步模型”得到的 `config_name`），或通过模型映射把公共模型名映射到这些名称。
3. 如需按量计费/扣减订阅额度，请为这些模型配置分组模型价格；未配置价格时调用成本会按 0 计。
4. 需要对外售卖/发放时，将分组设置为订阅分组，并配置订阅套餐。
5. 为用户签发 API Key 后，按 sub2api 的 OpenAI、Anthropic Messages 或 Responses 入口调用。

模型名使用 Trae 注册表中的 `config_name`，例如 `glm-5.3-flash`、`DeepSeek-V4-Pro`。如果配置了模型映射，可以使用自己的公共模型名映射到这些 `config_name`。

## 协议与限制

- 支持 Chat Completions、Anthropic Messages、Responses 三种入站协议。
- 上游使用 PAT 换取 `Cloud-IDE-JWT`，JWT 按 PAT 缓存，401 时自动失效并重试。
- 图片会下载并转换为 data URL；非多模态模型收到图片时直接返回 400。
- 当前不向上游转发 tools schema，工具调用只作为对话文本处理。依赖原生工具调用的 Agent 工作流暂时可能无法完成工具回合。
- Trae 没有公开的订阅余额/窗口查询接口，因此该渠道不提供余额或调度阈值自动探测。
