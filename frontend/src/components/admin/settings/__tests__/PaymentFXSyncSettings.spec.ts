import { flushPromises, mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import zhSettings from '@/i18n/locales/zh/admin/settings'
import enSettings from '@/i18n/locales/en/admin/settings'
import PaymentFXSyncSettings from '../PaymentFXSyncSettings.vue'

const mocks = vi.hoisted(() => ({
  get: vi.fn(),
  update: vi.fn(),
  sync: vi.fn(),
  groups: vi.fn(),
}))

vi.mock('@/api/admin/paymentFx', () => ({
  getPaymentFXSyncConfig: mocks.get,
  updatePaymentFXSyncConfig: mocks.update,
  syncPaymentFXNow: mocks.sync,
}))

vi.mock('@/api/admin/groups', () => ({
  getAllIncludingInactive: mocks.groups,
}))

const baseConfig = {
  enabled: true,
  urls: ['https://example.com/rates'],
  interval_hours: 24,
  markup_percent: 0,
  update_subscription_rate: false,
  last_success_at: '2026-11-10T12:00:00Z',
  last_attempt_at: '2026-11-10T12:00:00Z',
  last_rate: 6.686583,
  last_multiplier: 0.149553,
  last_source: 'https://example.com/rates',
}

function mountSettings() {
  return mount(PaymentFXSyncSettings, {
    global: {
      plugins: [createI18n({ legacy: false, locale: 'zh', messages: { zh: { admin: zhSettings }, en: { admin: enSettings } } })],
    },
  })
}

beforeEach(() => {
  vi.clearAllMocks()
  mocks.get.mockResolvedValue({ ...baseConfig })
  mocks.update.mockResolvedValue({ ...baseConfig })
  mocks.sync.mockResolvedValue({ ...baseConfig })
  mocks.groups.mockResolvedValue([
    { id: 2, name: '智谱GLM5.3' },
    { id: 6, name: '智谱GLM5.3-FLASH' },
  ])
})

describe('PaymentFXSyncSettings', () => {
  it('loads the configured rate and last sync state', async () => {
    const wrapper = mountSettings()
    await flushPromises()

    expect(mocks.get).toHaveBeenCalledOnce()
    expect(wrapper.text()).toContain('充值汇率自动同步')
    expect((wrapper.get('[data-test="urls"]').element as HTMLTextAreaElement).value).toBe('https://example.com/rates')
    expect((wrapper.get('[data-test="interval"]').element as HTMLInputElement).value).toBe('24')
    expect(wrapper.text()).toContain('0.149553')
    expect(wrapper.text()).toContain('6.686583')
  })

  it('saves the complete configuration with normalized URLs', async () => {
    const wrapper = mountSettings()
    await flushPromises()

    await wrapper.get('[data-test="urls"]').setValue('https://example.com/a\n, https://example.com/b\n')
    await wrapper.get('[data-test="interval"]').setValue('12')
    await wrapper.get('[data-test="markup"]').setValue('1.25')
    await wrapper.get('[data-test="save"]').trigger('click')
    await flushPromises()

    expect(mocks.update).toHaveBeenCalledWith({
      enabled: true,
      urls: ['https://example.com/a', 'https://example.com/b'],
      interval_hours: 12,
      markup_percent: 1.25,
      update_subscription_rate: false,
      model_prices: [],
    })
    expect(wrapper.text()).toContain('FX 同步设置已保存')
  })

  it('rejects enabling sync without an FX URL', async () => {
    mocks.get.mockResolvedValue({ ...baseConfig, urls: [] })
    const wrapper = mountSettings()
    await flushPromises()

    await wrapper.get('[data-test="save"]').trigger('click')
    await flushPromises()

    expect(mocks.update).not.toHaveBeenCalled()
    expect(wrapper.get('[role="alert"]').text()).toContain('至少填写一个汇率 API 地址')
  })

  it('blocks saving until the configuration is loaded and offers retry', async () => {
    mocks.get.mockRejectedValueOnce({ message: 'load failed' })
    const wrapper = mountSettings()
    await flushPromises()

    expect(wrapper.get('[data-test="save"]').attributes('disabled')).toBeDefined()
    expect(wrapper.get('[data-test="reload"]').exists()).toBe(true)
    expect(wrapper.get('[role="alert"]').text()).toContain('load failed')

    mocks.get.mockResolvedValueOnce({ ...baseConfig })
    await wrapper.get('[data-test="reload"]').trigger('click')
    await flushPromises()

    expect(wrapper.get('[data-test="save"]').attributes('disabled')).toBeUndefined()
    expect(wrapper.find('[data-test="reload"]').exists()).toBe(false)
    expect((wrapper.get('[data-test="urls"]').element as HTMLTextAreaElement).value).toBe('https://example.com/rates')
  })

  it('shows a sync failure and reloads the stored status', async () => {
    mocks.sync.mockRejectedValueOnce({ message: 'fx api unavailable' })
    const wrapper = mountSettings()
    await flushPromises()

    await wrapper.get('[data-test="sync"]').trigger('click')
    await flushPromises()

    expect(wrapper.get('[role="alert"]').text()).toContain('fx api unavailable')
    expect(mocks.get).toHaveBeenCalledTimes(2)
  })

  it('renders configured model prices with group options', async () => {
    mocks.get.mockResolvedValue({
      ...baseConfig,
      model_prices: [{ model: 'glm-5.3', cny_input: 8, cny_output: 28, cny_cache_read: 2, group_ids: [2] }],
    })
    const wrapper = mountSettings()
    await flushPromises()

    const rows = wrapper.findAll('[data-test="model-price-row"]')
    expect(rows).toHaveLength(1)
    expect((rows[0].get('[data-test="model-price-model"]').element as HTMLInputElement).value).toBe('glm-5.3')
    expect((rows[0].get('[data-test="model-price-input"]').element as HTMLInputElement).value).toBe('8')
    expect(rows[0].get('[data-test="model-price-groups"]').text()).toContain('智谱GLM5.3')
  })

  it('saves added model price entries', async () => {
    const wrapper = mountSettings()
    await flushPromises()

    await wrapper.get('[data-test="model-price-add"]').trigger('click')
    const row = wrapper.get('[data-test="model-price-row"]')
    await row.get('[data-test="model-price-model"]').setValue('kimi-k3')
    await row.get('[data-test="model-price-input"]').setValue('20')
    await row.get('[data-test="model-price-output"]').setValue('100')
    const select = row.get('[data-test="model-price-groups"]')
    ;(select.element as HTMLSelectElement).options[1].selected = true
    await select.trigger('change')
    await wrapper.get('[data-test="save"]').trigger('click')
    await flushPromises()

    expect(mocks.update).toHaveBeenCalledWith(
      expect.objectContaining({
        model_prices: [{ model: 'kimi-k3', cny_input: 20, cny_output: 100, cny_cache_read: 0, group_ids: [6] }],
      }),
    )
  })

  it('rejects incomplete model price entries', async () => {
    const wrapper = mountSettings()
    await flushPromises()

    await wrapper.get('[data-test="model-price-add"]').trigger('click')
    await wrapper.get('[data-test="save"]').trigger('click')
    await flushPromises()

    expect(mocks.update).not.toHaveBeenCalled()
    expect(wrapper.get('[role="alert"]').text()).toContain('模型价格条目')
  })
})
