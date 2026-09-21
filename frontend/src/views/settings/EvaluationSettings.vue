<template>
  <div class="evaluation-settings">
    <div class="section-header">
      <h2>{{ t('evaluation.title') }}</h2>
      <p class="section-description">{{ t('evaluation.description') }}</p>
    </div>

    <!-- 评测集固定在后端 dataset/samples 下，用户无法选择，所以先把这件事讲清楚，
         否则「开始评测」看起来像是会跑当前知识库的全部文档。 -->
    <div class="intro">
      <t-icon name="info-circle" class="intro-icon" />
      <div>
        <p class="intro-title">{{ t('evaluation.introTitle') }}</p>
        <p class="intro-desc">{{ t('evaluation.introDescription') }}</p>
      </div>
    </div>

    <div class="settings-group">
      <div class="setting-row">
        <div class="setting-info">
          <label>{{ t('evaluation.kbLabel') }}</label>
          <p class="desc">{{ t('evaluation.kbDescription') }}</p>
        </div>
        <div class="setting-control control-wide">
          <t-select
            v-model="form.knowledgeBaseId"
            :options="kbOptions"
            :loading="kbLoading"
            :disabled="running"
            filterable
            clearable
            :placeholder="t('evaluation.kbPlaceholder')"
          />
        </div>
      </div>

      <div class="setting-row">
        <div class="setting-info">
          <label>{{ t('evaluation.chatModelLabel') }}</label>
          <p class="desc">{{ t('evaluation.chatModelDescription') }}</p>
        </div>
        <div class="setting-control control-wide">
          <ModelSelector
            model-type="KnowledgeQA"
            :selected-model-id="form.chatModelId"
            :disabled="running"
            @update:selected-model-id="handleChatModelChange"
            @add-model="handleAddModel('chat')"
          />
        </div>
      </div>

      <div class="setting-row">
        <div class="setting-info">
          <label>{{ t('evaluation.rerankModelLabel') }}</label>
          <p class="desc">{{ t('evaluation.rerankModelDescription') }}</p>
        </div>
        <div class="setting-control control-wide">
          <ModelSelector
            model-type="Rerank"
            :selected-model-id="form.rerankModelId"
            :disabled="running"
            :clearable="true"
            @update:selected-model-id="handleRerankModelChange"
            @add-model="handleAddModel('rerank')"
          />
        </div>
      </div>
    </div>

    <div class="action-bar">
      <t-button theme="primary" :loading="running" :disabled="!canRun" @click="startEvaluation">
        {{ running ? t('evaluation.running') : t('evaluation.start') }}
      </t-button>
      <span v-if="!canRun && !running" class="action-hint">{{ t('evaluation.requiredHint') }}</span>
    </div>

    <!-- 任务状态：轮询期间显示进度，成功后展开指标 -->
    <div v-if="task || running" class="result-card">
      <div class="result-header">
        <span class="result-title">{{ t('evaluation.resultTitle') }}</span>
        <t-tag :theme="statusTheme" variant="light">
          {{ statusLabel }}
        </t-tag>
      </div>

      <div class="result-body">
        <div v-if="task" class="result-row">
          <span class="result-label">{{ t('evaluation.taskId') }}</span>
          <span class="result-value mono">{{ task.id }}</span>
        </div>

        <template v-if="running || task?.status === EVALUATION_STATUS.running">
          <t-progress
            theme="line"
            :percentage="progressPercent"
            :label="progressLabel"
          />
          <p class="result-hint">{{ t('evaluation.runningHint') }}</p>
        </template>

        <template v-if="task?.status === EVALUATION_STATUS.failed">
          <p class="result-error">{{ task.err_msg || t('evaluation.failedFallback') }}</p>
        </template>

        <template v-if="metric">
          <div class="metric-group">
            <p class="metric-group-title">{{ t('evaluation.retrievalMetrics') }}</p>
            <div class="metric-grid">
              <div v-for="item in retrievalMetricItems" :key="item.key" class="metric-cell">
                <span class="metric-name">{{ item.label }}</span>
                <span class="metric-value">{{ formatMetric(item.value) }}</span>
              </div>
            </div>
          </div>

          <div class="metric-group">
            <p class="metric-group-title">{{ t('evaluation.generationMetrics') }}</p>
            <div class="metric-grid">
              <div v-for="item in generationMetricItems" :key="item.key" class="metric-cell">
                <span class="metric-name">{{ item.label }}</span>
                <span class="metric-value">{{ formatMetric(item.value) }}</span>
              </div>
            </div>
          </div>
        </template>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'
import { MessagePlugin } from 'tdesign-vue-next'
import { useI18n } from 'vue-i18n'
import ModelSelector from '@/components/ModelSelector.vue'
import { useUIStore } from '@/stores/ui'
import { listKnowledgeBases } from '@/api/knowledge-base'
import {
  EVALUATION_STATUS,
  getEvaluationResult,
  runEvaluation,
  type EvaluationTask,
  type MetricResult,
} from '@/api/evaluation'

const { t } = useI18n()
const uiStore = useUIStore()

const form = reactive({
  knowledgeBaseId: '',
  chatModelId: '',
  rerankModelId: '',
})

const kbOptions = ref<Array<{ label: string; value: string }>>([])
const kbLoading = ref(false)
const running = ref(false)
const task = ref<EvaluationTask | null>(null)
const metric = ref<MetricResult | null>(null)

// 后端跑一次评测会调用真实模型（有成本），所以提交后要轮询到终态；
// 评测是长任务，2s 间隔既能给到进度感又不会把接口打满。
const POLL_INTERVAL = 2000
const POLL_TIMEOUT = 30 * 60 * 1000
const MAX_CONSECUTIVE_POLL_ERRORS = 5
let pollTimer: ReturnType<typeof setInterval> | null = null

const canRun = computed(
  () => !!form.knowledgeBaseId && !!form.chatModelId,
)

const progressPercent = computed(() => {
  const total = task.value?.total || 0
  const finished = task.value?.finished || 0
  if (total <= 0) return 0
  return Math.max(0, Math.min(100, Math.round((finished / total) * 100)))
})

const progressLabel = computed(() =>
  task.value?.total
    ? t('evaluation.progressLabel', {
        finished: task.value.finished || 0,
        total: task.value.total,
      })
    : t('evaluation.progressPending'),
)

const statusLabel = computed(() => {
  if (running.value && !task.value) return t('evaluation.statusStarting')
  switch (task.value?.status) {
    case EVALUATION_STATUS.success:
      return t('evaluation.statusSuccess')
    case EVALUATION_STATUS.failed:
      return t('evaluation.statusFailed')
    case EVALUATION_STATUS.running:
      return t('evaluation.statusRunning')
    default:
const POLL_INTERVAL = 2000
const POLL_TIMEOUT = 30 * 60 * 1000
const MAX_CONSECUTIVE_POLL_ERRORS = 5
      return t('evaluation.statusPending')
  }
})

const statusTheme = computed(() => {
  switch (task.value?.status) {
    case EVALUATION_STATUS.success:
      return 'success'
    case EVALUATION_STATUS.failed:
      return 'danger'
    case EVALUATION_STATUS.running:
      return 'primary'
    default:
      return 'default'
  }
})

const retrievalMetricItems = computed(() => {
  const m = metric.value?.retrieval_metrics
  if (!m) return []
  return [
    { key: 'precision', label: t('evaluation.metricPrecision'), value: m.precision },
    { key: 'recall', label: t('evaluation.metricRecall'), value: m.recall },
    { key: 'ndcg3', label: t('evaluation.metricNdcg3'), value: m.ndcg3 },
    { key: 'ndcg10', label: t('evaluation.metricNdcg10'), value: m.ndcg10 },
    { key: 'mrr', label: t('evaluation.metricMrr'), value: m.mrr },
    { key: 'map', label: t('evaluation.metricMap'), value: m.map },
  ]
})

const generationMetricItems = computed(() => {
  const m = metric.value?.generation_metrics
  if (!m) return []
  return [
    { key: 'bleu1', label: t('evaluation.metricBleu1'), value: m.bleu1 },
    { key: 'bleu2', label: t('evaluation.metricBleu2'), value: m.bleu2 },
    { key: 'bleu4', label: t('evaluation.metricBleu4'), value: m.bleu4 },
    { key: 'rouge1', label: t('evaluation.metricRouge1'), value: m.rouge1 },
    { key: 'rouge2', label: t('evaluation.metricRouge2'), value: m.rouge2 },
    { key: 'rougel', label: t('evaluation.metricRougeL'), value: m.rougel },
  ]
})

// 指标是 0~1 的小数，按百分比展示更直观；非数字一律显示占位符，
// 避免把 NaN 直接渲染到界面上。
const formatMetric = (value: unknown) => {
  const num = typeof value === 'number' ? value : Number(value)
  if (!Number.isFinite(num)) return '—'
  return `${(num * 100).toFixed(2)}%`
}

const loadKnowledgeBases = async () => {
  kbLoading.value = true
  try {
    const res: any = await listKnowledgeBases()
    const list = res?.data || []
    kbOptions.value = list.map((kb: any) => ({
      label: kb.name || kb.id,
      value: kb.id,
    }))
  } catch (error: any) {
    MessagePlugin.error(error?.message || t('evaluation.toasts.loadKbFailed'))
  } finally {
    kbLoading.value = false
  }
}

const stopPoll = () => {
  if (pollTimer) {
    clearInterval(pollTimer)
    pollTimer = null
  }
}

const startPoll = (taskId: string) => {
  stopPoll()
  let elapsed = 0
  let consecutiveErrors = 0
  pollTimer = setInterval(async () => {
    elapsed += POLL_INTERVAL
    // 评测任务只存在服务端内存里，进程重启后 task_id 就查不到了（接口会持续报错）。
    // 没有这个兜底，空 catch 会让定时器永远转下去、running 一直为 true，表单被永久锁住。
    if (elapsed > POLL_TIMEOUT) {
      stopPoll()
      running.value = false
      MessagePlugin.warning(t('evaluation.toasts.pollTimeout'))
      return
    }
    try {
      const res: any = await getEvaluationResult(taskId)
      consecutiveErrors = 0
      const detail = res?.data
      if (!detail?.task) return

      task.value = detail.task
      if (detail.metric) {
        metric.value = detail.metric
      }

      if (detail.task.status === EVALUATION_STATUS.success) {
        stopPoll()
        running.value = false
        MessagePlugin.success(t('evaluation.toasts.success'))
        return
      }

      if (detail.task.status === EVALUATION_STATUS.failed) {
        stopPoll()
        running.value = false
        MessagePlugin.error(detail.task.err_msg || t('evaluation.toasts.failed'))
      }
    } catch {
      // 单次查询失败不等于评测失败（可能是网络抖动），但连续失败说明任务已不可达
      // （例如服务重启后内存任务丢失），此时收敛比一直轮询更有用。
      consecutiveErrors += 1
      if (consecutiveErrors >= MAX_CONSECUTIVE_POLL_ERRORS) {
        stopPoll()
        running.value = false
        MessagePlugin.error(t('evaluation.toasts.pollLost'))
      }
    }
  }, POLL_INTERVAL)
}

const startEvaluation = async () => {
  if (!canRun.value || running.value) return
  running.value = true
  task.value = null
  metric.value = null
  stopPoll()
  try {
    const res: any = await runEvaluation({
      knowledge_base_id: form.knowledgeBaseId,
      chat_id: form.chatModelId,
      rerank_id: form.rerankModelId || undefined,
      // 不传 dataset_id：后端 DatasetService 目前只加载固定的 dataset/samples，
      // 该字段仅用于标记任务，传了也不会改变取用的评测集。
    })
    // 后端 POST /evaluation 返回的是 EvaluationDetail（{task, params, metric}），
    // 不是裸任务对象，任务 ID 在 data.task.id 上。
    const created: EvaluationTask | undefined = res?.data?.task
    if (res?.success && created?.id) {
      task.value = created
      MessagePlugin.info(t('evaluation.toasts.started'))
      startPoll(created.id)
    } else {
      running.value = false
      MessagePlugin.error(res?.message || t('evaluation.toasts.startFailed'))
    }
  } catch (error: any) {
    running.value = false
    MessagePlugin.error(error?.message || t('evaluation.toasts.startFailed'))
  }
}

const handleChatModelChange = (modelId: string) => {
  form.chatModelId = modelId
}

const handleRerankModelChange = (modelId: string) => {
  form.rerankModelId = modelId || ''
}

const handleAddModel = (subSection: 'chat' | 'rerank') => {
  uiStore.openSettings('models', subSection)
  window.dispatchEvent(
    new CustomEvent('settings-nav', { detail: { section: 'models', subsection: subSection } }),
  )
}

onMounted(loadKnowledgeBases)
onUnmounted(stopPoll)
</script>

<style lang="less" scoped>
.evaluation-settings {
  width: 100%;
}

.section-header {
  margin-bottom: 24px;

  h2 {
    font-size: 20px;
    font-weight: 600;
    color: var(--td-text-color-primary);
    margin: 0 0 8px 0;
  }

  .section-description {
    font-size: 14px;
    color: var(--td-text-color-secondary);
    margin: 0;
    line-height: 1.5;
  }
}

.intro {
  display: flex;
  align-items: flex-start;
  gap: 10px;
  padding: 14px 16px;
  margin-bottom: 8px;
  border-radius: 8px;
  background: var(--td-bg-color-secondarycontainer);
}

.intro-icon {
  color: var(--td-brand-color);
  margin-top: 2px;
  flex-shrink: 0;
}

.intro-title {
  margin: 0 0 4px 0;
  font-size: 14px;
  font-weight: 500;
  color: var(--td-text-color-primary);
}

.intro-desc {
  margin: 0;
  font-size: 13px;
  line-height: 1.6;
  color: var(--td-text-color-secondary);
}

.settings-group {
  display: flex;
  flex-direction: column;
}

.setting-row {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  padding: 20px 0;
  border-bottom: 1px solid var(--td-component-stroke);

  &:last-child {
    border-bottom: none;
  }
}

.setting-info {
  flex: 1;
  max-width: 60%;
  padding-right: 24px;

  label {
    font-size: 15px;
    font-weight: 500;
    color: var(--td-text-color-primary);
    display: block;
    margin-bottom: 4px;
  }

  .desc {
    font-size: 13px;
    color: var(--td-text-color-secondary);
    margin: 0;
    line-height: 1.5;
  }
}

.setting-control {
  flex-shrink: 0;
  display: flex;
  justify-content: flex-end;
  align-items: center;
}

.control-wide {
  width: 320px;
}

.action-bar {
  display: flex;
  align-items: center;
  gap: 12px;
  padding-top: 20px;
}

.action-hint {
  font-size: 13px;
  color: var(--td-text-color-placeholder);
}

.result-card {
  margin-top: 24px;
  border: 1px solid var(--td-component-stroke);
  border-radius: 8px;
  overflow: hidden;
}

.result-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 12px 16px;
  background: var(--td-bg-color-secondarycontainer);
}

.result-title {
  font-size: 14px;
  font-weight: 500;
  color: var(--td-text-color-primary);
}

.result-body {
  padding: 16px;
}

.result-row {
  display: flex;
  align-items: baseline;
  gap: 8px;
  margin-bottom: 12px;
}

.result-label {
  font-size: 13px;
  color: var(--td-text-color-secondary);
}

.result-value {
  font-size: 13px;
  color: var(--td-text-color-primary);
  word-break: break-all;
}

.mono {
  font-family: var(--td-font-family-mono, monospace);
}

.result-hint {
  margin: 8px 0 0 0;
  font-size: 12px;
  color: var(--td-text-color-placeholder);
}

.result-error {
  margin: 0;
  font-size: 13px;
  color: var(--td-error-color);
  line-height: 1.6;
}

.metric-group {
  margin-top: 16px;

  &:first-child {
    margin-top: 0;
  }
}

.metric-group-title {
  margin: 0 0 10px 0;
  font-size: 13px;
  font-weight: 500;
  color: var(--td-text-color-secondary);
}

.metric-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(150px, 1fr));
  gap: 10px;
}

.metric-cell {
  display: flex;
  flex-direction: column;
  gap: 4px;
  padding: 10px 12px;
  border-radius: 6px;
  background: var(--td-bg-color-secondarycontainer);
}

.metric-name {
  font-size: 12px;
  color: var(--td-text-color-secondary);
}

.metric-value {
  font-size: 16px;
  font-weight: 600;
  color: var(--td-text-color-primary);
}
</style>
