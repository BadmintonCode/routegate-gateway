# RouteGate admin skill — paste-ready AI bootstrap prompt

This file is a paste-ready bootstrap template for AI coding agents: paste one
of the two prompt blocks below into a fresh agent session and the agent will
install the `routegate-admin` skill **by downloading it from the gateway's
own admin API** (public endpoints, no credential needed for the skill files
themselves), configure the admin URL and a temporary token together with you,
and then operate the gateway API under the skill's safety rules. The template
is parameterized with a single placeholder, `{{ADMIN_URL}}`:

- The panel's 复制 AI 提示词 button (developer page → 接口文档) substitutes it
  with the address you are visiting — the copied prompt is ready as-is.
- If you use this file raw (or the served `/meta/prompt` / `/meta/skill/AI-PROMPT.md`
  copy), replace `{{ADMIN_URL}}` with your admin base URL before pasting.
- No repository checkout is required; one exists as a fallback for agents
  with filesystem access to this repository (`skills/routegate-admin/`).

## 中文模板（推荐直接粘贴）

```text
请安装并启用 RouteGate 管理端（routegate-admin）skill，之后严格按以下步骤操作：

1. 本管理端地址是：{{ADMIN_URL}}
   请将 ROUTEGATE_ADMIN_URL 环境变量设为该地址（后续所有步骤都用它）。
   若该地址不可达，向我确认正确的地址，绝不自行编造或尝试其他主机。
2. 安装 skill：该 skill 由网关自身公开托管（无需凭证）。用 IPv6 优先策略
   （先 curl -6 --connect-timeout 5，失败再 curl -4 --connect-timeout 5）
   依次下载以下三个文件，保存为 ~/.agents/skills/routegate-admin/ 下的
   同名文件（若你的工具使用其他 skill 目录，以你的为准）：
   {{ADMIN_URL}}/admin/api/v1/meta/skill/SKILL.md
   {{ADMIN_URL}}/admin/api/v1/meta/skill/ARCHITECTURE.md
   {{ADMIN_URL}}/admin/api/v1/meta/skill/AI-PROMPT.md
   然后完整阅读 SKILL.md 和 ARCHITECTURE.md 并严格遵循——先理解系统，
   再操作网关。（备用：若你能访问 RouteGate 仓库，文件在
   skills/routegate-admin/，内容相同。）
3. 与我一起把配置写入 .routegate/config.json（客户端约定，服务端不读）：
   顶层键为环境名（如 test /
   production），每个环境只记录 baseUrl、apiBasePath（/admin/api/v1）和
   auth: {"type":"bearer","tokenEnv":"ROUTEGATE_ADMIN_TOKEN"}——文件里只存
   令牌的环境变量名，绝不写令牌原文。保留文件中无关字段；不加 schema 版本
   或“当前环境”包装；不发明缺失的值，缺什么向我报告。确认该路径已被 git
   忽略（没有就把 .routegate/config.json 加进最近的 .gitignore）。写完
   校验 JSON，向我展示脱敏后的结构摘要并列出我还需提供的值。
   - ROUTEGATE_ADMIN_TOKEN（令牌本体只存环境变量）：由我在面板上铸造的
     临时令牌（开发者 → 我的凭证 → 新建凭证，建议有效期 7–30 天），绝不
     硬编码、提交、打印或回显。
4. 使用凭证之前先验证连通性：GET {{ADMIN_URL}}/admin/api/v1/meta/endpoints
   是公开接口（无需令牌），兼任连通性检查——按 SKILL.md 的 IPv6 优先策略
   请求。
5. 然后通过 skill 操作路由管理：列出路由（GET /routes）、读取网关快照、
   创建/更新/删除路由与策略。写操作一律使用 expected_revision 乐观并发
   （先 GET 拿到 config_revision），且任何写操作前必须先向我确认。
   遇 409 admin.revision_conflict：重新 GET 并用最新 config_revision 重试；
   遇 401 admin.token_invalid：停下，请我铸造新令牌。
6. 遵守 SKILL.md 的安全注意事项：写操作直接修改线上生效的网关配置。
```

## English equivalent

```text
Install the RouteGate admin (routegate-admin) skill, then follow these steps exactly:

1. The admin base URL is: {{ADMIN_URL}}
   Set the ROUTEGATE_ADMIN_URL environment variable to that address (every
   later step uses it). If it is unreachable, ask me for the correct address —
   never invent one or try other hosts.
2. Install the skill: the gateway hosts it publicly (no credential needed
   for the skill files). Using the IPv6-first strategy (curl -6
   --connect-timeout 5 first, then curl -4 --connect-timeout 5 on failure),
   download these three files and save them under
   ~/.agents/skills/routegate-admin/ (or your tool's skill directory):
   {{ADMIN_URL}}/admin/api/v1/meta/skill/SKILL.md
   {{ADMIN_URL}}/admin/api/v1/meta/skill/ARCHITECTURE.md
   {{ADMIN_URL}}/admin/api/v1/meta/skill/AI-PROMPT.md
   Then read SKILL.md AND ARCHITECTURE.md in full and follow them strictly —
   understand the system before operating it. (Fallback: with access to the
   RouteGate repository, the same files live at skills/routegate-admin/.)
3. Write the configuration into .routegate/config.json WITH me (client-side
   convention, server never reads it): top-level keys are environment names
   (e.g. test / production), each environment records only baseUrl, apiBasePath
   (/admin/api/v1), and auth: {"type":"bearer","tokenEnv":"ROUTEGATE_ADMIN_TOKEN"}
   — the file stores the token's ENVIRONMENT-VARIABLE NAME, never the token
   value. Preserve unrelated fields; do not add a schema version or an
   "active environment" wrapper; leave unknown values unset and report them.
   Make sure the path is git-ignored (add exactly .routegate/config.json
   to the nearest .gitignore if not). Validate the JSON, show me a REDACTED
   summary of the structure, and list the values I still must supply.
   - ROUTEGATE_ADMIN_TOKEN (the token itself lives only in the env var): a
     temporary token I mint in the panel (开发者 → 我的凭证 → 新建凭证;
     7-30 days recommended) — never hardcode, commit, print, or echo it.
4. Verify connectivity BEFORE using the credential: GET
   {{ADMIN_URL}}/admin/api/v1/meta/endpoints is PUBLIC (no token needed)
   and doubles as the connectivity check — request it with the skill's
   IPv6-first strategy.
5. Then operate route management through the skill: list routes (GET /routes),
   read the gateway snapshot, create/update/delete routes and policies. All
   writes use expected_revision optimistic concurrency (GET first); ALWAYS
   confirm with me before any write. On 409 admin.revision_conflict, re-GET
   and retry with the fresh config_revision; on 401 admin.token_invalid, stop
   and ask me to mint a new token.
6. Follow the skill's safety notes: writes mutate LIVE gateway configuration.
```
