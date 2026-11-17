<template>
  <div class="space-y-4 rounded-lg border border-gray-200 p-4 dark:border-dark-700">
    <div class="flex items-start justify-between gap-4">
      <div>
        <h3 class="font-semibold">{{ t('admin.settings.payment.fxSync.title') }}</h3>
        <p class="mt-1 text-xs text-gray-400">{{ t('admin.settings.payment.fxSync.description') }}</p>
      </div>
      <Toggle v-model="form.enabled" />
    </div>

    <p v-if="loading" class="text-sm text-gray-400">{{ t('admin.settings.payment.fxSync.loading') }}</p>
    <p v-if="error" role="alert" class="text-sm text-red-600 dark:text-red-400">{{ error }}</p>
    <p v-if="notice" role="status" class="text-sm text-green-600 dark:text-green-400">{{ notice }}</p>

    <label class="block">
      <span class="input-label">{{ t('admin.settings.payment.fxSync.urls') }}</span>
      <textarea
        v-model="urlsText"
        data-test="urls"
        rows="3"
        class="input mt-1"
        :placeholder="t('admin.settings.payment.fxSync.urlsPlaceholder')"
        :disabled="loading || saving || syncing || !loaded"
      ></textarea>
      <span class="mt-1 block text-xs text-gray-400">{{ t('admin.settings.payment.fxSync.urlsHint') }}</span>
    </label>

    <div class="grid gap-4 sm:grid-cols-2">
      <label class="block">
        <span class="input-label">{{ t('admin.settings.payment.fxSync.intervalHours') }}</span>
        <input
          v-model.number="form.interval_hours"
          data-test="interval"
          type="number"
          min="1"
          max="168"
          step="1"
          class="input mt-1"
          :disabled="loading || saving || syncing || !loaded"
        />
        <span class="mt-1 block text-xs text-gray-400">{{ t('admin.settings.payment.fxSync.intervalHoursHint') }}</span>
      </label>
      <label class="block">
        <span class="input-label">{{ t('admin.settings.payment.fxSync.markupPercent') }}</span>
        <input
          v-model.number="form.markup_percent"
          data-test="markup"
          type="number"
          min="0"
          max="100"
          step="0.01"
          class="input mt-1"
          :disabled="loading || saving || syncing || !loaded"
        />
        <span class="mt-1 block text-xs text-gray-400">{{ t('admin.settings.payment.fxSync.markupPercentHint') }}</span>
      </label>
    </div>

    <div class="flex items-center justify-between gap-4 rounded-lg border border-gray-200 px-3 py-2 dark:border-dark-700">
      <div>
        <p class="text-sm font-medium">{{ t('admin.settings.payment.fxSync.updateSubscriptionRate') }}</p>
        <p class="text-xs text-gray-400">{{ t('admin.settings.payment.fxSync.updateSubscriptionRateHint') }}</p>
      </div>
      <Toggle v-model="form.update_subscription_rate" />
    </div>

    <div class="space-y-2 rounded-lg border border-gray-200 px-3 py-2 dark:border-dark-700">
      <div>
        <p class="text-sm font-medium">{{ t('admin.settings.payment.fxSync.modelPricesTitle') }}</p>
        <p class="text-xs text-gray-400">{{ t('admin.settings.payment.fxSync.modelPricesHint') }}</p>
      </div>
      <div
        v-for="(row, index) in modelPrices"
        :key="index"
        data-test="model-price-row"
        class="grid items-end gap-2 sm:grid-cols-[1fr_1fr_1fr_1fr_1fr_auto]"
      >
        <label class="block">
          <span class="input-label">{{ t('admin.settings.payment.fxSync.modelPricesModel') }}</span>
          <input
            v-model="row.model"
            data-test="model-price-model"
            type="text"
            class="input mt-1"
            :disabled="loading || saving || syncing || !loaded"
          />
        </label>
        <label class="block">
          <span class="input-label">{{ t('admin.settings.payment.fxSync.modelPricesInput') }}</span>
          <input
            v-model.number="row.cny_input"
            data-test="model-price-input"
            type="number"
            min="0"
            step="0.0001"
            class="input mt-1"
            :disabled="loading || saving || syncing || !loaded"
          />
        </label>
        <label class="block">
          <span class="input-label">{{ t('admin.settings.payment.fxSync.modelPricesOutput') }}</span>
          <input
            v-model.number="row.cny_output"
            data-test="model-price-output"
            type="number"
            min="0"
            step="0.0001"
            class="input mt-1"
            :disabled="loading || saving || syncing || !loaded"
          />
        </label>
        <label class="block">
          <span class="input-label">{{ t('admin.settings.payment.fxSync.modelPricesCacheRead') }}</span>
          <input
            v-model.number="row.cny_cache_read"
            data-test="model-price-cache"
            type="number"
            min="0"
            step="0.0001"
            class="input mt-1"
            :disabled="loading || saving || syncing || !loaded"
          />
        </label>
        <label class="block">
          <span class="input-label">{{ t('admin.settings.payment.fxSync.modelPricesGroups') }}</span>
          <select
            v-model="row.group_ids"
            data-test="model-price-groups"
            multiple
            size="3"
            class="input mt-1"
            :disabled="loading || saving || syncing || !loaded"
          >
            <option v-for="group in groups" :key="group.id" :value="group.id">{{ group.name }}</option>
          </select>
        </label>
        <button
          type="button"
          data-test="model-price-remove"
          class="btn btn-secondary"
          :disabled="loading || saving || syncing || !loaded"
          @click="modelPrices.splice(index, 1)"
        >
          {{ t('admin.settings.payment.fxSync.modelPricesRemove') }}
        </button>
      </div>
      <button
        type="button"
        data-test="model-price-add"
        class="btn btn-secondary"
        :disabled="loading || saving || syncing || !loaded"
        @click="addModelPrice"
      >
        {{ t('admin.settings.payment.fxSync.modelPricesAdd') }}
      </button>
    </div>

    <div class="flex flex-wrap gap-2">
      <button type="button" data-test="save" class="btn btn-primary" :disabled="loading || saving || syncing || !loaded" @click="save">
        {{ saving ? t('admin.settings.payment.fxSync.saving') : t('admin.settings.payment.fxSync.save') }}
      </button>
      <button type="button" data-test="sync" class="btn btn-secondary" :disabled="loading || saving || syncing || !loaded" @click="syncNow">
        {{ syncing ? t('admin.settings.payment.fxSync.syncing') : t('admin.settings.payment.fxSync.syncNow') }}
      </button>
      <button v-if="!loading && !loaded" type="button" data-test="reload" class="btn btn-secondary" @click="load">
        {{ t('admin.settings.payment.fxSync.retry') }}
      </button>
    </div>

    <div v-if="status?.last_success_at || status?.last_attempt_at || status?.last_error" class="space-y-1 border-t border-gray-200 pt-3 text-xs text-gray-500 dark:border-dark-700 dark:text-gray-400">
      <p class="font-medium text-gray-700 dark:text-gray-300">{{ t('admin.settings.payment.fxSync.statusTitle') }}</p>
      <p v-if="status?.last_success_at">{{ t('admin.settings.payment.fxSync.lastSuccess', { value: formatTime(status.last_success_at) }) }}</p>
      <p v-if="status?.last_attempt_at">{{ t('admin.settings.payment.fxSync.lastAttempt', { value: formatTime(status.last_attempt_at) }) }}</p>
      <p v-if="status?.last_rate">{{ t('admin.settings.payment.fxSync.lastRate', { value: formatNumber(status.last_rate) }) }}</p>
      <p v-if="status?.last_multiplier">{{ t('admin.settings.payment.fxSync.lastMultiplier', { value: formatNumber(status.last_multiplier) }) }}</p>
      <p v-if="status?.last_subscription_rate">{{ t('admin.settings.payment.fxSync.lastSubscriptionRate', { value: formatNumber(status.last_subscription_rate) }) }}</p>
      <p v-if="status?.last_source">{{ t('admin.settings.payment.fxSync.lastSource', { value: status.last_source }) }}</p>
      <p v-if="status?.next_run_at">{{ t('admin.settings.payment.fxSync.nextRun', { value: formatTime(status.next_run_at) }) }}</p>
      <p v-if="status?.last_error" class="text-red-600 dark:text-red-400">{{ t('admin.settings.payment.fxSync.lastError', { value: status.last_error }) }}</p>
    </div>
  </div>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import Toggle from '@/components/common/Toggle.vue'
import { extractApiErrorMessage } from '@/utils/apiError'
import {
  getPaymentFXSyncConfig,
  syncPaymentFXNow,
  updatePaymentFXSyncConfig,
  type PaymentFXModelPrice,
  type PaymentFXSyncConfig,
} from '@/api/admin/paymentFx'
import { getAllIncludingInactive } from '@/api/admin/groups'

const { t } = useI18n()

const form = reactive({
  enabled: false,
  interval_hours: 24,
  markup_percent: 0,
  update_subscription_rate: false,
})
const urlsText = ref('')
const modelPrices = ref<PaymentFXModelPrice[]>([])
const groups = ref<{ id: number; name: string }[]>([])
const status = ref<PaymentFXSyncConfig | null>(null)
const loading = ref(true)
const loaded = ref(false)
const saving = ref(false)
const syncing = ref(false)
const error = ref('')
const notice = ref('')

function splitURLs(): string[] {
  return urlsText.value
    .split(/\r?\n|,/)
    .map((item) => item.trim())
    .filter(Boolean)
}

function applyConfig(cfg: PaymentFXSyncConfig) {
  status.value = cfg
  form.enabled = cfg.enabled
  form.interval_hours = cfg.interval_hours || 24
  form.markup_percent = cfg.markup_percent || 0
  form.update_subscription_rate = cfg.update_subscription_rate
  urlsText.value = (cfg.urls || []).join('\n')
  modelPrices.value = (cfg.model_prices || []).map((entry) => ({
    model: entry.model,
    cny_input: entry.cny_input,
    cny_output: entry.cny_output,
    cny_cache_read: entry.cny_cache_read || 0,
    group_ids: [...(entry.group_ids || [])],
  }))
  loaded.value = true
}

function addModelPrice() {
  modelPrices.value.push({ model: '', cny_input: 0, cny_output: 0, cny_cache_read: 0, group_ids: [] })
}

function collectModelPrices(): PaymentFXModelPrice[] | null {
  const entries: PaymentFXModelPrice[] = []
  for (const row of modelPrices.value) {
    const model = row.model.trim()
    const input = Number(row.cny_input)
    const output = Number(row.cny_output)
    const cacheRead = Number(row.cny_cache_read || 0)
    if (
      !model ||
      !Number.isFinite(input) || input <= 0 ||
      !Number.isFinite(output) || output <= 0 ||
      !Number.isFinite(cacheRead) || cacheRead < 0 ||
      row.group_ids.length === 0
    ) {
      return null
    }
    entries.push({ model, cny_input: input, cny_output: output, cny_cache_read: cacheRead, group_ids: [...row.group_ids] })
  }
  return entries
}

async function load() {
  loading.value = true
  loaded.value = false
  error.value = ''
  try {
    applyConfig(await getPaymentFXSyncConfig())
  } catch (err) {
    error.value = extractApiErrorMessage(err, t('admin.settings.payment.fxSync.loadFailed'))
  } finally {
    loading.value = false
  }
  try {
    groups.value = (await getAllIncludingInactive()).map((group) => ({ id: group.id, name: group.name }))
  } catch {
    // Group options are best-effort; the config editor still works without them.
  }
}

async function save() {
  error.value = ''
  notice.value = ''
  const urls = splitURLs()
  if (form.enabled && urls.length === 0) {
    error.value = t('admin.settings.payment.fxSync.urlRequired')
    return
  }
  const interval = Math.trunc(Number(form.interval_hours))
  if (!Number.isFinite(interval) || interval < 1 || interval > 168) {
    error.value = t('admin.settings.payment.fxSync.invalidInterval')
    return
  }
  const markup = Number(form.markup_percent)
  if (!Number.isFinite(markup) || markup < 0 || markup > 100) {
    error.value = t('admin.settings.payment.fxSync.invalidMarkup')
    return
  }
  const entries = collectModelPrices()
  if (entries === null) {
    error.value = t('admin.settings.payment.fxSync.modelPricesInvalid')
    return
  }

  saving.value = true
  try {
    const cfg = await updatePaymentFXSyncConfig({
      enabled: form.enabled,
      urls,
      interval_hours: interval,
      markup_percent: Math.round(markup * 100) / 100,
      update_subscription_rate: form.update_subscription_rate,
      model_prices: entries,
    })
    applyConfig(cfg)
    notice.value = t('admin.settings.payment.fxSync.saved')
  } catch (err) {
    error.value = extractApiErrorMessage(err, t('admin.settings.payment.fxSync.saveFailed'))
  } finally {
    saving.value = false
  }
}

async function syncNow() {
  error.value = ''
  notice.value = ''
  syncing.value = true
  try {
    applyConfig(await syncPaymentFXNow())
    notice.value = t('admin.settings.payment.fxSync.syncSuccess')
  } catch (err) {
    error.value = extractApiErrorMessage(err, t('admin.settings.payment.fxSync.syncFailed'))
    try {
      applyConfig(await getPaymentFXSyncConfig())
    } catch {
      // Keep the operation error visible if the refresh also fails.
    }
  } finally {
    syncing.value = false
  }
}

function formatTime(value?: string): string {
  if (!value) return t('admin.settings.payment.fxSync.never')
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString()
}

function formatNumber(value?: number): string {
  if (value === undefined || value === null || !Number.isFinite(value) || value === 0) return '-'
  return value.toFixed(6).replace(/\.?0+$/, '')
}

onMounted(load)
</script>
