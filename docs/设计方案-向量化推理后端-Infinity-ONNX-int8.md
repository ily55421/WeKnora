# 设计方案：向量化推理后端优化（Infinity + ONNX int8）

> 版本：v1.0（2026-09-06）｜ 状态：设计稿，未实施
> 前置分析：本文是《向量化瓶颈分析》中方案③的详细设计。目标读者：本机 WeKnora Lite 部署的维护者。
> 约束继承：无 Docker、无 NVIDIA 独显（i7-12700H / 64GB / AVX2）、尽量零系统污染、HuggingFace 直连不可用。

---

## 一、背景与目标

### 1.1 现状（已实测）

| 指标 | 现值 | 出处 |
|---|---|---|
| 向量化吞吐 | **2.37s/chunk**（384 字符子块，bge-m3 F16 via Ollama CPU） | Trace span embedding，38 chunks / 90.3s |
| 50KB 文档全流程 | 192s（62 chunks），其中向量化 ~188s | DB knowledge_processing_spans |
| 阶段占比 | 向量化 ≈88% | 解析 61ms + 分块 15ms + 向量化 90s + 后处理 12s |
| 并发有效性 | 无效——Ollama 单 runner 串行消化，WeKnora 侧 3 并发排队等价于串行 | batch.go 8 批 × ants 3 并发 ↔ 实测 38×2.37s 吻合 |

### 1.2 瓶颈本质

CPU + GGML（Ollama）跑 568M 参数 F16 模型的物理推理速度，已达该运行时上限。参数层微调（`BATCH_EMBED_SIZE`、`OLLAMA_NUM_PARALLEL`）无实质空间。

### 1.3 目标

- **主目标**：向量化吞吐提升至 **≤400ms/chunk**（≥6 倍），20KB 文档入库从 90s → **≤15s**
- 保持 bge-m3 dense 向量语义（1024 维），检索质量无可感知退化
- 不引入 Docker；服务可纳入现有 `start-weknora-stack.ps1` 一键管理
- 可一键回滚（Ollama 栈原样保留）

---

## 二、方案总览

### 2.1 架构对比

**Before（现状）**
```
WeKnora ── /api/embed (Ollama 协议) ──> Ollama :11434 ── GGML F16 CPU 串行 ──> 2.37s/chunk
```

**After（本方案）**
```
WeKnora ── /v1/embeddings (OpenAI 兼容) ──> Infinity :7997 ── ONNX Runtime int8 CPU
                                                                        ├── 动态批处理（batching window）
                                                                        └── 专用 tokenization worker 线程
                                                                        → 预期 150~400ms/chunk
```

### 2.2 组件清单

| 组件 | 选型 | 角色 |
|---|---|---|
| 推理服务 | **Infinity**（michaelfeil/infinity，`infinity_emb v2` CLI） | OpenAI 兼容 embedding 服务端，动态 batching，官方 CPU 推荐引擎 `optimum`（ONNX Runtime） |
| 模型 | **bge-m3 dense encoder**（BAAI/bge-m3 的 XLMRoberta 部分） | 保持 1024 维 dense 语义；WeKnora 仅消费 dense 向量，sparse/colbert 头不参与 |
| 模型权重获取 | **ModelScope**（`modelscope download`，国内直连） | 绕开 HuggingFace 封锁 |
| 推理格式 | **ONNX int8**（`quantize_dynamic` 权重量化） | CPU 上 ONNX Runtime int8 是 x86 最优解（Infinity 官方基准：int8 比 torch 快 ~2.5x，ONNX 为 "best in class for CPU on intel/amd"） |
| WeKnora 接入 | **OpenAI 兼容 embedder**（`internal/models/embedding/embedder.go` → `NewOpenAIEmbedder`，remote source 默认路由） | 零代码改动，UI 配置切换 |

### 2.3 技术选型对比（为什么是 Infinity）

| 候选 | Windows 无 Docker | CPU int8 | OpenAI 兼容 | 动态批处理 | 结论 |
|---|---|---|---|---|---|
| Infinity (pip) | ✅ 原生 | ✅ optimum+int8 | ✅ | ✅ | **选定** |
| TEI (HF text-embeddings-inference) | ❌ 仅 Docker/Linux | ✅ | ✅ | ✅ | 违反无 Docker 约束，排除 |
| vLLM | ❌ 不支持 Windows 原生 | ❌ | ✅ | ✅ | 排除 |
| sentence-transformers + FastAPI 自建 | ✅ | ⚠️ 需自实现 | 需写代码 | ❌ | 可控性最差，排除 |
| 保持 Ollama | ✅ | ❌ 无官方量化 embedding 模型 | ✅ | ❌ | 现状，作为回滚基线 |

---

## 三、关键设计决策与坑规避

### 3.1 【坑①】bge-m3 不是标准 transformers 架构 —— 导出策略

BAAI/bge-m3 的 HF 仓库是 FlagEmbedding 自定义类（`BGEM3Model`，多输出 dict：dense+sparse+colbert）。`optimum-cli export onnx` 无法直接导出该自定义类。

**规避设计**：只导出 dense encoder 部分——bge-m3 的 dense 输出等价于其 XLMRoberta 主干 + 归一化池化：

```python
from transformers import XLMRobertaModel
model = XLMRobertaModel.from_pretrained("<本地 bge-m3 目录>")
# 走 optimum --task feature-extraction 导出（last_hidden_state / mean-pooling 由服务端处理）
```

验证依据：bge-m3 的 dense_vecs = XLMRoberta encoder last_hidden 的 CLS-pooling + L2 normalize（FlagEmbedding 源码行为）。**语义一致性验证**（见 §6.3）：抽样若干文本，对比 ONNX 导出向量 vs FlagEmbedding 官方 dense 向量的 cosine 相似度矩阵，要求 >0.999。

> WeKnora 侧确认（代码证据 `engines.go` / 检索链路）：仅消费 dense 1024 维，sparse/colbert 不入库 → 放弃两个 head 无功能损失。

### 3.2 【坑②】HuggingFace 不可用 —— 模型获取路径

- 下载：`uv tool install modelscope` → `modelscope download --model BAAI/bge-m3 --local_dir <dir>`（ModelScope 国内 CDN 直连）
- 后续 optimum/onnxruntime 全部**本地离线**操作（`--model <本地路径>`），不再触网
- Infinity 启动亦用 `--model-id <本地路径>`，禁用 hub 拉取

### 3.3 【坑③】int8 精度风险 —— 分档设计

| 档位 | 文件 | 预期吞吐（i7-12700H） | 质量风险 |
|---|---|---|---|
| FP32 ONNX（保底档） | `model.onnx` | ~600-1000ms/chunk（仍比现状快 2-4x） | 无（数值等价） |
| **int8 ONNX（目标档）** | `model_quantized.onnx` | **~150-400ms/chunk** | MTEB 典型掉点 0.5-2%，cosine 排序基本不变 |

`quantize_dynamic`（weight-only，激活 FP16/FP32）是 embedding 模型的保守量化法。设计上**两档产物都保留**，部署时先跑 FP32 验证链路，再切 int8 跑质量对比（§6.3），两步走隔离问题。

### 3.4 【坑④】Infinity optimum 引擎对模型目录的要求

Infinity optimum 引擎要求模型目录内含：`config.json` + tokenizer 全套（`tokenizer.json` / `sentencepiece.bpe.model` 等）+ `model.onnx`（int8 时替换或用 `--served-model-name` 指定）。设计为独立目录：

```
E:\models\bge-m3-dense-onnx\     ← 模型根（E 盘，145GB 空闲）
├── config.json                   ← 从 ModelScope 下载目录复制（XLMRobertaConfig）
├── tokenizer.json / sentencepiece.bpe.model / special_tokens_map.json ...
├── model.onnx                    ← FP32 导出产物（保底档）
└── model_quantized.onnx          ← int8 产物；启用时重命名为 model.onnx 放入子目录 int8\
```

目录放 E 盘独立于仓库（`E:\models\`），与 Tools 哲学一致（不污染仓库与系统盘）。

### 3.5 资源与占用控制

| 项 | 设计值 | 说明 |
|---|---|---|
| 常驻内存 | ~1.5GB（int8 权重 0.6GB + ORt 运行时 + Infinity 框架） | 64GB 内存无压力 |
| CPU 线程 | ORt 默认全核；可经 `--engine optimum` 配套线程参数收敛 | 入库为突发负载，全核可用；若需限制参考附录 B |
| 端口 | **7997**（Infinity 示例惯用端口；部署前探测冲突） | SSRF 白名单 127.0.0.1 已覆盖 |
| 进程形态 | `uv tool` 隔离环境 + 分离式 Start-Process + 日志重定向 | 与 Ollama/docreader 同款模式 |

---

## 四、部署设计

### 4.1 模型准备（一次性）

```powershell
# 1) 安装 ModelScope CLI（uv tool 隔离）
uv tool install modelscope

# 2) 下载 bge-m3（约 2.3GB，国内直连）
modelscope download --model BAAI/bge-m3 --local_dir E:\models\bge-m3-hf

# 3) 独立 venv 做 ONNX 导出与量化（一次性工作环境，用完可删）
uv venv E:\models\onnx-export-env
uv pip install --python E:\models\onnx-export-env optimum[exporters] onnxruntime onnx transformers torch --index-url <国内 PyPI 镜像按需>

# 4) 导出 FP32 ONNX（只取 XLMRoberta dense encoder）
#    optimum-cli export onnx -m E:\models\bge-m3-hf --task feature-extraction E:\models\bge-m3-dense-onnx
#    （若 optimum 对该 repo 的自定义 config 报错，回退手工脚本：XLMRobertaModel 加载 + torch.onnx.export，
#      见附录 A；导出后复制 tokenizer 文件进目录）

# 5) int8 动态量化（weight-only）
#    python -c "from onnxruntime.quantization import quantize_dynamic, QuantType; quantize_dynamic('model.onnx','model_quantized.onnx',weight_type=QuantType.QInt8)"
#    （多行版本见附录 A，PowerShell 下先写 .py 再跑）
```

### 4.2 Infinity 服务部署

```powershell
# 1) 安装（uv tool 隔离，零全局污染）
uv tool install "infinity-emb[all]"

# 2) 启动（分离式，纳入栈管理）
infinity_emb v2 `
  --engine optimum `
  --model-id E:\models\bge-m3-dense-onnx `
  --served-model-name bge-m3 `
  --port 7997 `
  --batch-delay 50ms          # 动态批聚合窗口，入库场景建议 50-100ms
```

启动后自检：`curl http://127.0.0.1:7997/models`（应列出 bge-m3）。

### 4.3 WeKnora 接入（UI 操作，零代码）

「设置 → 模型管理 → 添加模型」：

| 字段 | 值 | 依据 |
|---|---|---|
| 模型类型 | Embedding | — |
| API 类型 | **OpenAI 兼容**（remote source；provider 留空走 `NewOpenAIEmbedder` 默认路由，`embedder.go` L160+） | 代码确认：remote 未匹配专属 provider 时默认 OpenAI 兼容 |
| 模型名 | `bge-m3`（须与 `--served-model-name` 一致） | — |
| Base URL | `http://127.0.0.1:7997`（Infinity OpenAI 兼容层：`/v1/embeddings`） | SSRF 白名单已含 127.0.0.1 ✓ |
| API Key | 任意非空（如 `local`；Infinity 未启用鉴权时忽略） | 表单必填则填占位 |
| 维度 | 1024 | 与现库向量一致 |

**存量数据策略**：bge-m3 权重同源，向量语义一致，理论上可直接混用；但 int8 引入微小数值漂移，且切换 KB 的 embedding_model_id 后统一来源更干净——**建议对存量文档执行一次"批量重新解析"**（38 chunks 文档预计秒级完成，成本极低）。

### 4.4 栈集成

`start-weknora-stack.ps1` 增加第 4 个服务段（模式与 Ollama 相同）：

```powershell
# ---- 1.5) Infinity embedding server (:7997) ----
if (Test-Listening 7997) { Write-Host '[SKIP ] Infinity already running' }
else {
  Start-Process -FilePath 'infinity_emb' -ArgumentList 'v2','--engine','optimum',
      '--model-id','E:\models\bge-m3-dense-onnx','--served-model-name','bge-m3',
      '--port','7997','--batch-delay','50ms' `
      -WorkingDirectory 'E:\models' -WindowStyle Hidden `
      -RedirectStandardOutput 'E:\models\infinity.out.log' `
      -RedirectStandardError  'E:\models\infinity.err.log' | Out-Null
  Write-Host '[START] Infinity launching...'
}
Wait-Port 'Infinity' 7997 30
```

（顺序置于 Ollama 之后、docreader 之前均可；失败不阻塞主栈。）

### 4.5 Ollama 的去留

**保留**。理由：① 回滚通道（KB 模型随时切回 Ollama bge-m3）；② 用户后续拉对话 LLM 时仍需 Ollama。Embedding 流量切换后 Ollama 处于空闲（模型 5min keep_alive 后自动卸载，零常驻开销）。

---

## 五、性能验证方案

### 5.1 基准方法

固定基准集 = 现库文档《知识图谱知识功能点总结.md》（38 chunks），从 UI 触发"重新解析"，Trace span `embedding` 的 `duration_ms` 为准（口径与现状数据完全一致）。

### 5.2 指标与验收

| 指标 | 现状基线 | 验收门槛（int8 档） | 超预期 |
|---|---|---|---|
| embedding span 总耗时 | 90,255ms | **≤15,000ms** | ≤8,000ms |
| 单 chunk 吞吐 | 2.37s | ≤400ms | ≤210ms |
| 检索功能回归 | — | 对基准文档提问，检索引用正确、Top-3 命中与切换前一致 | — |

若 int8 档仅达成 FP32 档水平（~2-4x），仍优于现状，可接受并记录；同时保留 FP32 档产物供诊断。

### 5.3 质量对比（切换前后）

抽样 10 组查询词 × 5 篇文档：对比 Infinity-int8 与 Ollama-F16 输出向量的 cosine 相似度矩阵相关系数（应 >0.995），并人工抽检 Top-3 检索结果排序一致性。

---

## 六、回滚方案

1. UI「模型管理」将 KB 的 Embedding 模型切回 Ollama bge-m3（原配置原样保留，全程未动）
2. 对存量文档"批量重新解析"（回滚侧成本同优化侧，秒-分钟级）
3. Infinity 服务可停（不进栈脚本或注释掉该段即可）；模型文件保留无副作用

**零破坏性**：本方案不修改 WeKnora 代码、不改 Ollama、不动数据库 schema；全部变更在"新增服务 + UI 配置"层。

---

## 七、风险清单

| # | 风险 | 概率 | 缓解 |
|---|---|---|---|
| 1 | optimum 导出 bge-m3 因自定义 config 失败 | 中 | 附录 A 手工 torch.onnx.export 兜底（XLMRobertaModel 标准 forward）；极端情况退 torch 引擎（社区已验证 infinity torch engine 跑 bge-m3，但提速降为 ~1.5-2x） |
| 2 | int8 检索质量退化超预期 | 低 | 分档设计：FP32 档保底（本身也有 2-4x）；§5.3 质量门禁 |
| 3 | Infinity optimum 引擎 Windows 成熟度（官方 CI 以 Linux 为主） | 中 | 安装后先跑 FP32 冒烟；若引擎初始化失败，退路① torch 引擎，退路② 放弃方案③保持 Ollama（零损失） |
| 4 | ModelScope 权重与 HF 版本/文件差异（如缺 sentencepiece 分词） | 低 | 下载后核对文件清单（附录 A）；tokenizer 文件可从 ModelScope 同 repo 补齐 |
| 5 | ONNX 8192 长度动态 shape 稳定性 | 低 | WeKnora 子块 384 字符远小于上限；导出时 `--fp16`/动态轴参数按附录 A 校验 |
| 6 | 导出/量化脚本环境（torch CPU 版 ~2GB） | 确定性成本 | 独立一次性 venv（`E:\models\onnx-export-env`），完成可删 |

---

## 八、实施步骤清单（总览）

```
Phase 0  环境准备     uv tool install modelscope / infinity-emb[all]；建 E:\models
Phase 1  模型获取     modelscope download BAAI/bge-m3（~2.3GB）
Phase 2  ONNX 导出    optimum 导出 FP32（兜底：手工脚本）；tokenizer 文件归位
Phase 3  int8 量化    quantize_dynamic；两档产物分目录
Phase 4  服务冒烟     启动 Infinity（先 FP32 档）→ /v1/embeddings 返回 1024 维校验
Phase 5  接入切换     UI 配置新 Embedding 模型 → 基准文档 reparse → §5 验收
Phase 6  档位切换     换 int8 档 → 重跑基准 + 质量对比 → 达标定稿
Phase 7  栈集成       更新 start-weknora-stack.ps1；更新启动文档
```

预计总工时：1-2.5 小时（含模型下载与两次基准）。磁盘新增：~6GB（HF 权重 2.3 + ONNX 两档 ~1.2×2 + 导出环境 2GB，后者可删）。

---

## 附录 A：导出与量化参考脚本

```python
# export_onnx.py —— optimum 失败时的手工兜底（dense encoder, 动态 seq 长度）
import torch
from transformers import XLMRobertaModel, AutoTokenizer

SRC = r"E:\models\bge-m3-hf"
DST = r"E:\models\bge-m3-dense-onnx"
model = XLMRobertaModel.from_pretrained(SRC, torch_dtype=torch.float32)
model.eval()
dummy = torch.randint(0, 250000, (1, 8))  # XLM-R vocab 250002
with torch.no_grad():
    torch.onnx.export(
        model, (dummy,), f"{DST}\\model.onnx",
        input_names=["input_ids"], output_names=["last_hidden_state"],
        dynamic_axes={"input_ids": {0: "batch", 1: "seq"},
                      "last_hidden_state": {0: "batch", 1: "seq"}},
        opset_version=17,
    )
tok = AutoTokenizer.from_pretrained(SRC)
tok.save_pretrained(DST)   # tokenizer 全套落位
```

```python
# quantize_int8.py —— weight-only 动态量化
from onnxruntime.quantization import quantize_dynamic, QuantType
quantize_dynamic(r"E:\models\bge-m3-dense-onnx\model.onnx",
                 r"E:\models\bge-m3-dense-onnx\model_quantized.onnx",
                 weight_type=QuantType.QInt8)
```

> 注意：powerShell 下多行 Python 一律先写 .py 文件再运行（本机已知约束）。
> mean-pooling 归一化：Infinity 服务端按 `sentence_transformers` 池化约定处理 feature-extraction 输出；若冒烟向量与 Ollama 输出 cosine <0.99，需核对池化方式（CLS vs mean），此时改用附录 A 显式封装 pooled 输出再导出。

## 附录 B：常用运维命令

```powershell
# 启动/自检
infinity_emb v2 --engine optimum --model-id E:\models\bge-m3-dense-onnx --served-model-name bge-m3 --port 7997
curl http://127.0.0.1:7997/models
# 向量维度自检（应输出 1024）
curl.exe -s http://127.0.0.1:7997/v1/embeddings -H "Content-Type: application/json" -d '{"model":"bge-m3","input":"维度自检"}'
# CPU 限制（可选）：ORt 线程数经环境变量 OMP_NUM_THREADS / ORT_* 控制，入库突发时默认全核可接受
```

## 附录 C：与既有分析文档的关系

- 瓶颈数据来源：《向量化瓶颈分析》（对话内交付，2026-09-06）：2.37s/chunk、88% 占比、Ollama 单 runner 串行
- 本方案与方案①（BATCH_EMBED_SIZE 微调）、方案②（分块 384→768）**正交可叠加**；若本方案落地，①②的边际收益趋零，可不实施
- 上游约束回顾：`internal/models/embedding/embedder.go`（OpenAI 兼容默认路由）、`batch.go`（BATCH_EMBED_SIZE/ants 池）、`engines.go`（仅消费 dense）
