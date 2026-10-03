import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import EmailPreview from '../components/EmailPreview.vue'
import StateBadge from '../components/StateBadge.vue'
import type { Preview } from '../types'

const base: Preview = { from: 'a@x.test', to: ['b@y.test'], subject: 'Hi' }

describe('StateBadge', () => {
  it('shows a readable label and a state hook', () => {
    const w = mount(StateBadge, { props: { state: 'retry_scheduled' } })
    expect(w.text()).toBe('Retry scheduled')
    expect(w.attributes('data-state')).toBe('retry_scheduled')
    expect(w.classes()).toContain('warn')
  })
})

describe('EmailPreview security', () => {
  const hostile = '<script>alert(1)</script><img src="https://tracker.test/p.gif" onerror="alert(2)"><b>bold</b>'

  it('renders stored HTML only inside a fully sandboxed iframe', () => {
    const w = mount(EmailPreview, { props: { preview: { ...base, html: hostile } } })
    const frame = w.find('iframe')
    expect(frame.exists()).toBe(true)
    // An empty sandbox attribute disables scripts, forms, popups and same-origin access.
    expect(frame.attributes('sandbox')).toBe('')
    expect(frame.attributes('referrerpolicy')).toBe('no-referrer')
    expect(frame.attributes('srcdoc')).toContain('<b>bold</b>')
    // Nothing from the email may leak into this page's own DOM.
    expect(w.element.querySelector('script')).toBeNull()
    expect(w.element.querySelector('img')).toBeNull()
  })

  it('escapes plain text and header values', () => {
    const w = mount(EmailPreview, { props: { preview: { ...base, subject: '<i>x</i>', text: '<b>not bold</b>' } } })
    expect(w.find('pre').text()).toBe('<b>not bold</b>')
    expect(w.element.querySelector('b')).toBeNull()
    expect(w.element.querySelector('i')).toBeNull()
  })

  it('switches between HTML and text', async () => {
    const w = mount(EmailPreview, { props: { preview: { ...base, html: '<p>h</p>', text: 'plain' } } })
    expect(w.find('iframe').exists()).toBe(true)
    await w.findAll('[role=tab]')[1].trigger('click')
    expect(w.find('iframe').exists()).toBe(false)
    expect(w.find('pre').text()).toBe('plain')
  })

  it('lists attachments and handles empty bodies', () => {
    const w = mount(EmailPreview, { props: { preview: { ...base, attachments: [{ filename: 'a.pdf', content_type: 'application/pdf', size: 2048 }] } } })
    expect(w.text()).toContain('a.pdf')
    expect(w.text()).toContain('2.0 KB')
    expect(w.text()).toContain('no body')
  })
})
