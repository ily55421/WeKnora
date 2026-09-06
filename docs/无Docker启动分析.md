# WeKnora 无 Docker 启动分析

> 分析对象：`E:\code\source\WeKnora`（Tencent/WeKnora，v0.8.0，HEAD `3d3bb7f6`）
> 分析目标：不使用 Docker，如何在（Windows）本机直接启动完整服务
> 所有结论均标注代码/配置出处，可直接核对。

---

## 一、TL;DR

**WeKnora 官方内置"零外部依赖"的 Lite 模式：单二进制 + SQLite + 内存队列 + 内嵌前端，是完全无 Docker 启动的正解。** 核心公式：

```
go build -tags "sqlite_fts5" (CGO_ENABLED=1, EDITION=lite)  +  .env.lite  →  一个进程跑全套
```

标准 Docker 部署的 5 个组件在 Lite 模式下全部被进程内实现替代：

| Docker 组件 | Lite 模式替代 | 开关 |
|---|---|---|
| PostgreSQL (ParadeDB) | SQLite（WAL + FTS5 全文 + sqlite-vec 向量） | `DB_DRIVER=sqlite` |
| Redis (Asynq 队列 + 流) | 内存流管理器 + SyncTaskExecutor 内联派发 | `STREAM_MANAGER_TYPE=memory` 且 `REDIS_ADDR` 留空 |
| frontend (Nginx) | Go 进程直接托管 `./web` 静态目录（SPA） | `web/index.html` 存在即自动生效 |
| docreader (Python gRPC) | **可选**：不跑则仅支持简单格式；跑则本机 Python 进程直连 | `DOCREADER_ADDR=127.0.0.1:50051` |
| Ollama / 外部 LLM | **必须外接**（本地 Ollama 或任意 OpenAI 兼容 API） | UI 内配置或 `OLLAMA_BASE_URL` |

**Windows 本机唯一的硬缺口是 gcc：SQLite（`sqlite_fts5`）与数据分析 Agent 的 DuckDB 均为 CGO 依赖，任何版本编译都需要 mingw-w64。** Go 1.26.0 / Node 22 / uv 均已就绪。

---

## 二、项目架构与组件拆解

### 2.1 标准部署形态（docker-compose.yml）

```
用户 → frontend(Nginx :80) → app(Go :8080)
                              ├── PostgreSQL :5432   (paradedb/paradedb:v0.22.2-pg17，数据 + BM25 + 向量)
                              ├── Redis :6379         (Asynq 任务队列 + 流状态)
                              ├── docreader :50051    (Python gRPC 文档解析)
                              └── 宿主机 Ollama :11434 (host.docker.internal)
```

- **app**（Go 1.26，`cmd/server/main.go`）：HTTP API + RAG/Agent 推理 + 任务编排，依赖注入容器 `internal/container/container.go`（1797 行，约 150 个路由）
- **docreader**（Python 3.10 + uv，`docreader/`）：gRPC 文档解析服务（markitdown / opendataloader-pdf / playwright 等）
- **frontend**（Vue3 + TDesign + Vite，`frontend/`）：SPA，Nginx 托管并反代 `/api/`
- 可选 profile：MinIO / Neo4j / Qdrant / Milvus / SearXNG / Langfuse / MCP 等，均不影响核心运行

### 2.2 逐组件"去容器化"可行性（代码级证据）

| 组件 | 去容器化方式 | 出处 |
|---|---|---|
| 数据库 | `DB_DRIVER=sqlite` → gorm SQLite 方言；`DB_PATH` 缺省 `./data/weknora.db`，自动建目录，WAL 模式，`sqlite_vec.Auto()` 加载向量扩展，单连接串行化防 `database is locked` | `internal/container/container.go` L639-700, L777-781 |
| 数据库迁移 | `AUTO_MIGRATE != "false"` 时启动即跑 `migrations/sqlite/*.sql`（000000_init ~ 000013，共 14 组）；迁移失败仅告警不阻断启动 | `container.go` L736-759；`internal/database/migration.go` L100-137 |
| 检索引擎 | `RETRIEVE_DRIVER=sqlite` → 注册 `KVHybridRetrieveEngine`（FTS5 关键词 + sqlite-vec 向量混合检索） | `container.go` L1120-1134；`internal/application/repository/retriever/sqlite/` |
| 任务队列 | `REDIS_ADDR` 为空 → 不注册任何 Asynq client/server，改注册 `SyncTaskExecutor`（内联 goroutine 派发）+ `NoopTaskInspector` + 进程内并发治理器 | `container.go` L316-340 |
| 对话流管理 | `STREAM_MANAGER_TYPE != "redis"` → `NewMemoryStreamManager()`（默认即内存，无需显式设置） | `internal/stream/factory.go` |
| 文件存储 | `STORAGE_TYPE=local` → 本地磁盘 `LOCAL_STORAGE_BASE_DIR=./data/files` | `.env.lite.example` |
| 前端托管 | Go 侧 `serveFrontendStatic` 中间件：`WEKNORA_WEB_DIR`（缺省 `./web`）下存在 `index.html` 即托管 SPA，`/api/`、`/health` 等路径自动豁免；必须在鉴权中间件之前注册 | `internal/router/static.go` L14-46 |
| 版本标识 | 编译期 ldflags 注入 `handler.Edition`（lite/standard），控制 `POST /auth/auto-setup` 等桌面端专属能力 | `internal/handler/auth.go` L895；`scripts/get_version.sh` |
| Neo4j 图谱 | 默认关闭（`NEO4J_ENABLE=false`），不装即无影响 | `.env.lite.example` |

### 2.3 文档解析引擎矩阵（决定 docreader 是否必须）

`internal/infrastructure/docparser/engines.go` 注册 8 种解析引擎，关键三种：

| 引擎 | 支持格式 | 依赖 |
|---|---|---|
| `simple`（Go 原生） | md / markdown / txt / csv / json / jpg / png / gif / bmp / tiff / webp / mp3 / wav / m4a / flac / ogg | **零依赖，永远可用** |
| `builtin`（docreader） | docx / doc / pdf / xlsx / xls / pptx / ppt / epub / html / mhtml / xmind + 图片/音频 | 需 gRPC 连通 docreader（`CheckAvailable` 检查 `docreaderConnected`） |
| `anydoc`（进程内 Rust） | Office 系（docx/xlsx/pptx 等） | 需 Rust 工具链编译静态库（`make anydoc-lib`）+ `-tags anydoc` 链接；`anydoc.Available()` 运行时探测 |

引擎选择逻辑（`preferAnydocWhenAvailable`）：anydoc 已链接且支持该格式 → 优先 anydoc；简单格式始终走 Go 原生 simple。

**结论：不跑 docreader、不编 anydoc 时，只能入库 md/txt/csv/json/图片/音频；要 PDF/Office 就必须二选一补上。**

---

## 三、无 Docker 启动路线对比

| | 路线 A：Lite 单二进制 ★推荐 | 路线 B：标准版原生多进程 |
|---|---|---|
| 外部进程数 | 1（WeKnora.exe，可选 +docreader +Ollama） | 5+（PG + Redis + docreader + app + 前端/Ollama） |
| 数据库 | SQLite（零安装） | 自装 PostgreSQL/ParadeDB 17 |
| 队列 | 进程内内存 | 自装 Redis 7 |
| 功能差异 | Wiki/任务队列/多引擎检索/多空间 RBAC 均可用；沙箱后端按空间另配（Cube/E2B 云端或本机 Docker）；无分布式横向扩展 | 与 docker compose 完全等价 |
| 数据迁移 | `migrations/sqlite/` 自动跑 | `migrations/paradedb/` + `migrations/versioned/` 自动跑（AUTO_MIGRATE） |
| Windows 成本 | 装 gcc → 4 条命令 | 装 PG + Redis + Python 依赖，最重 |
| 官方文档 | `website-docs/01-getting-started/02-installation.md` §7.1 | 同文档 §八 + `docs/开发指南.md` |

> 注意：`make dev-start`（开发模式）**仍用 Docker 起基础设施**（postgres/redis/docreader 容器），不属于无 Docker 方案。

---

## 四、路线 A：Lite 模式（Windows 本机完整步骤）

### 4.0 前置条件检查（本机实测 2026-09-06）

| 工具 | 要求 | 本机状态 |
|---|---|---|
| Go | 1.26（`go.mod` / Dockerfile builder） | ✅ go1.26.0 windows/amd64 |
| Node.js | 18+（前端构建） | ✅ v22.22.2 |
| gcc (mingw-w64) | CGO 编译 `mattn/go-sqlite3`（`go.mod` 传递依赖）+ `sqlite_fts5` tag | ❌ **缺失，必须安装** |
| Python 3.10+ + uv | 仅 docreader 可选依赖 | ✅ Python 3.11.0b4 / uv 0.11.19 |
| Git Bash | Makefile/脚本为 bash 语法 | ✅ 已装 |

**安装 gcc（三选一）**：
```powershell
# 方式1：MSYS2（推荐，包管理方便）
winget install MSYS2.MSYS2
# 然后在 MSYS2 终端: pacman -S mingw-w64-x86_64-gcc，并把 C:\msys64\mingw64\bin 加入 PATH

# 方式2：w64devkit（最轻量，解压即用）
# 从 GitHub brendonbeans/w64devkit 下载 zip，解压后 bin 目录加入 PATH

# 方式3：WinLibs（独立 mingw-w64 发行版）
```

### 4.1 构建前端并部署到 web/

```powershell
cd E:\code\source\WeKnora\frontend
$env:NODE_ENV='development'   # 本机全局 NODE_ENV=production 会跳过 devDependencies！
npm ci                         # 或 npm install
npm run build                  # 产出 frontend/dist

cd ..
Remove-Item -Recurse -Force web -ErrorAction SilentlyContinue
Copy-Item -Recurse frontend/dist web    # Go 二进制从 ./web 托管（static.go）
```

> 一次性工作：之后改后端代码重复构建时无需再动前端（等效 `SKIP_FRONTEND=1`）。

### 4.2 编译 Lite 版二进制

PowerShell 手动版（不依赖 make，最稳）：

```powershell
cd E:\code\source\WeKnora
$env:CGO_ENABLED='1'
go build -tags "sqlite_fts5" `
  -ldflags "-X 'github.com/Tencent/WeKnora/internal/handler.Edition=lite' -X 'github.com/Tencent/WeKnora/internal/handler.Version=0.8.0'" `
  -o WeKnora-lite.exe ./cmd/server
```

Git Bash 版（等效 `make build-lite SKIP_FRONTEND=1`）：

```bash
export EDITION=lite
eval "$(./scripts/get_version.sh ldflags)"   # 生成完整 ldflags（含 COMMIT_ID/BUILD_TIME）
CGO_ENABLED=1 go build -tags "sqlite_fts5" -ldflags="-w -s $LDFLAGS" -o WeKnora-lite.exe ./cmd/server
```

### 4.3 准备 .env.lite 配置

```powershell
Copy-Item .env.lite.example .env.lite
```

**必改两处**（编辑 `.env.lite`）：

```ini
SYSTEM_AES_KEY=<32字符随机串，如 openssl rand -hex 16 生成 32 个字符>   # ⚠️ 见下方坑①
JWT_SECRET=<随机串>
```

其余保持模板默认即可（`DB_DRIVER=sqlite`、`RETRIEVE_DRIVER=sqlite`、`STREAM_MANAGER_TYPE=memory`、`STORAGE_TYPE=local`、`DOCREADER_ADDR=127.0.0.1:50051`）。

### 4.4 启动（加载 .env.lite 环境变量）

PowerShell 没有等效 `set -a && source .env.lite`，逐行导出：

```powershell
# 方式1：用 Git Bash 一键启动（推荐）
bash -c "set -a && . ./.env.lite && set +a && ./WeKnora-lite.exe"

# 方式2：PowerShell 逐变量导出（示例，按 .env.lite 实际内容补全）
$env:DB_DRIVER='sqlite'; $env:DB_PATH='./data/weknora.db'
$env:RETRIEVE_DRIVER='sqlite'; $env:STREAM_MANAGER_TYPE='memory'
$env:STORAGE_TYPE='local'; $env:LOCAL_STORAGE_BASE_DIR='./data/files'
$env:JWT_SECRET='<同上>'; $env:SYSTEM_AES_KEY='<同上>'
$env:OLLAMA_BASE_URL='http://127.0.0.1:11434'
.\WeKnora-lite.exe
```

### 4.5 验证与初始化

```powershell
curl http://localhost:8080/health        # 后端健康检查
# 浏览器打开 http://localhost:8080        # 前端 SPA 由 Go 直接托管（无需 Nginx）
```

- 首次访问注册页正常注册即可；Lite 专属 `POST /auth/auto-setup` 可一键生成本地账号（`internal/handler/auth.go` L895，仅 `Edition=lite` 开放）
- 启动日志确认三行关键输出：`DB Config: driver=sqlite path=...`、`Running database migrations...`、`Serving frontend static files from ...\web`
- LLM 配置：UI「设置」内填任意 OpenAI 兼容 API（DeepSeek/智谱/StepFun 等），或本机跑 `ollama serve` 后自动发现

### 4.6 可选：本机跑 docreader（解锁 PDF/Office 解析）

proto 文件已预生成（`docreader/proto/docreader_pb2.py` 等），无需再跑 generate_proto：

```powershell
cd docreader
uv sync                      # 按 uv.lock 装依赖（requires-python >=3.10.18，本机 3.11 满足）
uv run python main.py        # gRPC 监听 127.0.0.1:50051（DOCREADER_GRPC_PORT 默认）
```

局限（相对官方 Docker 镜像）：镜像额外内置 LibreOffice/OpenJDK/antiword，裸机跑时 `.doc`（老格式）、部分 PPT 转换质量可能下降；`playwright` 需另跑 `playwright install webkit`（HTML 渲染用）。不跑 docreader 时应用照常启动，只是上传 PDF/Word 会提示解析引擎不可用。

### 4.7 可选：anydoc 进程内 Office 解析（免 Python 进程）

需要 Rust 工具链（`make anydoc-lib` 编译 `third_party/anydoc-go` 静态库），再 `go build -tags "anydoc,sqlite_fts5"`。链接后 `preferAnydocWhenAvailable` 自动让 Office 文档优先走进程内解析，可完全替代 docreader 的 Office 部分（PDF 仍需 docreader/云端引擎）。

---

## 五、路线 B：标准版原生多进程（功能完整等价）

适合需要 PostgreSQL 全文/向量能力（pgvector HNSW）、分布式队列、或与生产环境完全一致的场景。

1. **PostgreSQL**：安装 PostgreSQL 17（官方镜像用 paradedb/paradedb v0.22.2-pg17 = PG17 + BM25 + 向量扩展；普通 PG 17 + pgvector + pg_trgm 亦可，迁移文件在 `migrations/paradedb/` 与 `migrations/versioned/`），创建库 `weknora`
2. **Redis 7**：Windows 侧用 Memurai / WSL Redis / 旧版 redis-windows 移植，开启 `--requirepass` + `--appendonly yes`
3. **docreader**：同 4.6
4. **配置**：`cp .env.example .env`，把 `DB_HOST=localhost`、`REDIS_ADDR=localhost:6379`、`DOCREADER_ADDR=localhost:50051`、`OLLAMA_BASE_URL=http://127.0.0.1:11434` 改为本机地址；`RETRIEVE_DRIVER=postgres`、`STORAGE_TYPE=local`（本地磁盘免 MinIO）
5. **启动**：`go build -o WeKnora.exe ./cmd/server` 后带 .env 环境变量运行（或 `go run ./cmd/server`）；`AUTO_MIGRATE` 默认 true 自动建表
6. **前端**：开发用 `cd frontend && npm run dev`（Vite :5173 代理 API）；生产用 4.1 的 `web/` 方式由 Go 直接托管

> **标准版同样需要 gcc**：数据分析 Agent 直接依赖 `github.com/duckdb/duckdb-go/v2`（CGO，go.mod L19，自带含 windows-amd64 在内的预编译静态库）。`make build-prod` 即为 `CGO_ENABLED=1`。无 gcc 时标准版与 Lite 版同样无法编译。

---

## 六、坑清单（本机/本仓库实测发现）

| # | 坑 | 说明与规避 |
|---|---|---|
| ① | **`.env.lite.example` 中 `TENANT_AES_KEY` 是过时变量名** | Go 主应用 v0.4.0 起只读 `SYSTEM_AES_KEY`（docker-compose.yml 注释明示"已废弃，Go 不再读取"；全仓非测试代码仅 `SYSTEM_AES_KEY`）。照模板填旧名不生效：加密字段（模型 API Key、存储凭证等）将以无主密钥状态落库，UI 显示为空需重填。**务必写 `SYSTEM_AES_KEY`（32 字节）** |
| ② | **Windows 无 gcc → 任何版本 CGO 构建必败** | `sqlite_fts5` tag 属 `mattn/go-sqlite3`（CGO）；数据分析 Agent 的 `duckdb-go/v2` 也是 CGO 直接依赖。不装 mingw-w64 时报 `gcc: executable file not found`。装完记得新开终端让 PATH 生效 |
| ③ | **本机全局 `NODE_ENV=production`** | `npm ci` 会静默跳过 devDependencies（vite 等），`npm run build` 直接失败。前置 `$env:NODE_ENV='development'` |
| ④ | **Makefile / scripts/*.sh 全为 bash 语法** | PowerShell 下不可用 `make build-lite`。用 Git Bash，或按 4.2-4.4 的手动等效命令。`run-lite` 内的 `set -a && . ./.env.lite` 也是 bash 特性 |
| ⑤ | **SQLite 单连接串行化** | Lite 模式 `SetMaxOpenConns(1)`（防锁库），高并发入库吞吐有上限，属预期设计而非故障 |
| ⑥ | **Ollama 默认地址在容器/本机不同** | `.env` 通用模板是 `host.docker.internal:11434`，本机直跑必须改成 `127.0.0.1:11434`（`.env.lite.example` 已正确） |
| ⑦ | **前端不放 `web/` 时 UI 打不开** | `static.go` 探测不到 `web/index.html` 会静默跳过静态托管（日志无前端服务行），只剩裸 API。这是设计而非报错 |
| ⑧ | **docreader 裸跑格式支持弱于官方镜像** | 镜像内置 LibreOffice/OpenJDK/antiword；`uv sync` 裸跑处理 `.doc` 老格式等可能异常，可接受则无视 |

---

## 七、关键事实速查表

| 事实 | 出处 | 性质 |
|---|---|---|
| Lite = 编译期 `EDITION=lite`（ldflags 注入 `handler.Edition`）+ 运行期 `.env.lite` | `scripts/get_version.sh`；`Makefile` build-lite；`internal/handler/auth.go` L895 | ✅ 已验证 |
| SQLite 路径缺省 `./data/weknora.db`，自动建目录，WAL + busy_timeout=5000 + FK on | `internal/container/container.go` L684-695 | ✅ 已验证 |
| `AUTO_MIGRATE` 默认开启（`!= "false"` 即跑），迁移失败不阻断启动 | `container.go` L736-759 | ✅ 已验证 |
| SQLite 迁移文件齐全（14 组 up/down） | `migrations/sqlite/` | ✅ 已验证 |
| `REDIS_ADDR` 为空 → SyncTaskExecutor 内联 goroutine + NoopTaskInspector + 进程内并发治理 | `container.go` L316-340 | ✅ 已验证 |
| `STREAM_MANAGER_TYPE` 缺省即内存流（仅显式 = redis 才用 Redis） | `internal/stream/factory.go` | ✅ 已验证 |
| 前端托管条件：`./web/index.html` 存在（或 `WEKNORA_WEB_DIR` 指定），先于鉴权中间件注册 | `internal/router/static.go` L14-46 | ✅ 已验证 |
| 无 docreader 时仅 md/txt/csv/json/图片/音频可入库（simple 引擎）；复杂格式需 docreader/anydoc/云引擎 | `internal/infrastructure/docparser/engines.go` | ✅ 已验证 |
| docreader proto 已预生成，`uv sync` 后可直接 `python main.py` | `docreader/proto/`（pb2 文件在库） | ✅ 已验证 |
| Lite 专属 `POST /auth/auto-setup` 一键本地账号 | `internal/handler/auth.go` L895 | ✅ 已验证 |
| 本机工具链：Go 1.26.0 ✅ / Node 22.22.2 ✅ / uv 0.11.19 ✅ / **gcc ❌** | 实测 `go version` 等 | ✅ 已验证 |
| `.env.lite.example` 的 `TENANT_AES_KEY` 过时，应改用 `SYSTEM_AES_KEY`（32 字节） | 全仓代码仅读 SYSTEM_AES_KEY | ⚠️ 模板缺陷 |
| **标准版构建同样需要 CGO/gcc**——数据分析 Agent 直接依赖 duckdb-go/v2（CGO 库，go.mod L19，自带各平台预编译静态库含 windows-amd64）；Makefile `build-prod` 亦为 CGO_ENABLED=1 | `go.mod`；`Makefile` build-prod | ⚠️ 已修正 |
| `make dev-start` 开发模式仍依赖 Docker 起基础设施，非无 Docker 方案 | `docs/开发指南.md`；`scripts/dev.sh` | ⚠️ 易误解 |
