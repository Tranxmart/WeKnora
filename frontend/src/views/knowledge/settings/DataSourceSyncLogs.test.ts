import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'
import { runInNewContext } from 'node:vm'
import test from 'node:test'
import { compileScript, compileTemplate, parse } from '@vue/compiler-sfc'
import ts from 'typescript'
import { createRenderer, defineComponent, h, nextTick, ref } from 'vue'
import { createI18n } from 'vue-i18n'
import en from '../../../i18n/locales/en-US'
import zh from '../../../i18n/locales/zh-CN'

const require = createRequire(import.meta.url)
const filename = fileURLToPath(new URL('./DataSourceSyncLogs.vue', import.meta.url))
const { descriptor } = parse(readFileSync(filename, 'utf8'), { filename })
const sfc = compileScript(descriptor, { id: 'datasource-sync-logs-test' })
const script = sfc.content.replace('__expose();', '')
  .replace('return __returned__', '__expose(__returned__); return __returned__')
const template = compileTemplate({
  source: descriptor.template!.content, filename, id: 'datasource-sync-logs-test',
  compilerOptions: { bindingMetadata: sfc.bindings },
})
assert.deepEqual(template.errors, [])
const compiled = ts.transpileModule(`${script}\n${template.code}`, {
  compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
}).outputText

interface Node {
  text: string
  children: Node[]
  parent?: Node
}

async function fixture(locale: 'en-US' | 'zh-CN') {
  const errors = [
    { title: 'Empty.md', code: 'document_service_empty_file' },
    { title: 'Large.pdf', code: 'file_too_large' },
    { title: 'Program.exe', code: 'unsupported_file_type' },
  ]
  const log = {
    id: 'partial-run', data_source_id: 'managed-source', status: 'partial',
    started_at: '2026-11-16T10:00:00Z', finished_at: '2026-11-16T10:01:00Z',
    items_total: 7, items_created: 2, items_updated: 1, items_deleted: 0,
    items_skipped: 1, items_failed: 3, error_message: '', result: { errors },
  }
  const i18n = createI18n({
    legacy: false, locale, fallbackLocale: locale,
    messages: { 'en-US': en, 'zh-CN': zh },
  })
  const exports: any = {}
  runInNewContext(compiled, {
    exports, console,
    require(name: string) {
      if (name === 'vue') return require('vue')
      if (name === 'vue-i18n') return { useI18n: () => i18n.global }
      if (name === '@/api/datasource') return { getSyncLogs: async () => ({ data: [log] }) }
      return { __esModule: true, default: { render: () => null } }
    },
  })
  const component = exports.default
  component.render = exports.render
  const node = (text = ''): Node => ({ text, children: [] })
  const remove = (child: Node) => {
    if (!child.parent) return
    const siblings = child.parent.children
    const index = siblings.indexOf(child)
    if (index >= 0) siblings.splice(index, 1)
    child.parent = undefined
  }
  const renderer = createRenderer<Node, Node>({
    createElement: () => node(), createText: text => node(text), createComment: () => node(),
    insert(child, parent, anchor) {
      remove(child)
      child.parent = parent
      const index = anchor ? parent.children.indexOf(anchor) : -1
      if (index >= 0) parent.children.splice(index, 0, child)
      else parent.children.push(child)
    },
    remove,
    setElementText(element, text) { element.text = text; element.children = [] },
    setText(element, text) { element.text = text },
    patchProp() {}, parentNode: node => node.parent || null,
    nextSibling(node) { return node.parent?.children[node.parent.children.indexOf(node) + 1] || null },
  })
  const instance = ref<any>()
  const app = renderer.createApp({
    render: () => h(component, { visible: true, dataSourceId: 'managed-source', dataSourceName: 'Tranxmart Documents', ref: instance }),
  })
  // The drawer and control shells do not affect the actual log content under test.
  for (const name of ['t-drawer', 't-tooltip', 't-button', 't-icon', 't-loading']) {
    app.component(name, defineComponent({ setup: (_, { slots }) => () => h('div', slots.default?.()) }))
  }
  const root = node()
  app.mount(root)
  await instance.value.fetchLogs()
  instance.value.toggleExpand('partial-run')
  await nextTick()
  const content = (element: Node): string => [element.text, ...element.children.map(content)].join(' ')
  return { vm: instance.value, text: content(root), errors, t: i18n.global.t, close: () => app.unmount() }
}

for (const locale of ['en-US', 'zh-CN'] as const) {
  test(`partial Document Service runs display counts and localized failures in ${locale}`, async () => {
    const f = await fixture(locale)
    try {
      assert.ok(f.text.includes(f.t('datasource.logStatus.partial')), f.text)
      assert.ok(f.text.includes('+2'), f.text)
      assert.ok(f.text.includes('~1'), f.text)
      assert.ok(f.text.includes(f.t('datasource.logDetail.docsFailedSummary', { n: 3 })), f.text)
      for (const error of f.errors) {
        const reason = f.t(`datasource.syncError.${error.code}`)
        assert.ok(f.text.includes(`${error.title} — ${reason}`), f.text)
        assert.ok(!f.text.includes(error.code), f.text)
      }
      assert.equal(f.vm.stats.totalItems, 3)
      assert.equal(f.vm.stats.success, 0)
      assert.equal(f.vm.stats.failed, 0)
    } finally { f.close() }
  })
}
