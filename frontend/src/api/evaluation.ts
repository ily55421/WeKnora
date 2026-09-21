import { get, post } from '@/utils/request'

/**
 * 评测（Evaluation）后端契约。
 *
 * 后端只有两个端点（internal/router/routes_infra.go）：
 *   - POST /api/v1/evaluation  → 提交一次评测，立即返回任务（Admin+）
 *   - GET  /api/v1/evaluation  → 按 task_id 取结果（Viewer+）
 *
 * 评测集不是上传的：后端固定读取仓库内 dataset/samples/*.parquet，
 * dataset_id 留空即使用默认集（service/evaluation.go 中
 * `if datasetID == "" { datasetID = "default" }`），所以前端只让用户选
 * 知识库 + 对话模型 + 重排模型。
 */

/** 提交评测的入参。字段名与后端 EvaluationRequest 的 json tag 一一对应。 */
export interface EvaluationRequest {
  /** 留空表示使用后端默认评测集（./dataset/samples）。 */
  dataset_id?: string
  knowledge_base_id: string
  /** 对话模型 ID，后端字段名为 chat_id。 */
  chat_id: string
  /** 重排模型 ID，后端字段名为 rerank_id。 */
  rerank_id?: string
}

/** 任务状态。与 types.EvaluationStatue 的取值一致（0-3）。 */
export const EVALUATION_STATUS = {
  pending: 0,
  running: 1,
  success: 2,
  failed: 3,
} as const

export interface EvaluationTask {
  id: string
  tenant_id: number
  dataset_id: string
  start_time: string
  status: number
  err_msg?: string
  total?: number
  finished?: number
}

export interface RetrievalMetrics {
  precision: number
  recall: number
  ndcg3: number
  ndcg10: number
  mrr: number
  map: number
}

export interface GenerationMetrics {
  bleu1: number
  bleu2: number
  bleu4: number
  rouge1: number
  rouge2: number
  rougel: number
}

export interface MetricResult {
  retrieval_metrics: RetrievalMetrics
  generation_metrics: GenerationMetrics
}

export interface EvaluationDetail {
  task: EvaluationTask
  /** 评测用的检索/对话参数快照，结构由后端 ChatManage 决定，前端只做兜底展示。 */
  params?: Record<string, unknown>
  metric?: MetricResult
}

/** 提交评测任务。 */
export function runEvaluation(payload: EvaluationRequest) {
  return post('/api/v1/evaluation', payload)
}

/**
 * 查询评测结果。
 *
 * 注意 task_id 是 query 参数（后端 GetEvaluationRequest 用的是 form tag），
 * 不是路径参数。
 */
export function getEvaluationResult(taskId: string) {
  return get(`/api/v1/evaluation?task_id=${encodeURIComponent(taskId)}`)
}
