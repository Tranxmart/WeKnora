import assert from 'node:assert/strict'
import test, { type TestContext } from 'node:test'
import { fixture, flush } from '../../test-utils/vueFixture.ts'
import * as imageProcessing from '../../utils/imageProcessingConfig.ts'
import * as modelDefaults from '../../utils/modelDefaults.ts'

async function setup(t: TestContext, saved?: Record<string, unknown>) {
  const f = fixture(t)
  const props = { visible: true, mode: saved ? 'edit' : 'create', kbId: saved ? 'kb' : '' }
  const page = f.component(
    new URL('./KnowledgeBaseEditorModal.vue', import.meta.url),
    {
      'vue-i18n': { useI18n: () => ({ t: (key: string) => key, te: () => false }) },
      'tdesign-vue-next': { MessagePlugin: {}, DialogPlugin: {} },
      '@/config/contextualGuides': { KB_EDITOR_FOCUS_SECTION_EVENT: 'kb-focus', markContextualGuideDone() {} },
      '@/composables/useModalShell': { useModalShell: () => ({ markClean() {} }) },
      '@/api/knowledge-base': {
        getKnowledgeBaseById: async () => ({ data: { id: 'kb', type: 'document', ...saved } }),
        listKnowledgeFiles: async () => ({ total: 0 }),
        fetchImageAttrSchema: async () => ({ attributes: [], default_actions: { ocr: { on: [] } } }),
        FALLBACK_IMAGE_ATTR_SCHEMA: { attributes: [], default_actions: { ocr: { on: [] } } },
        mergeImageActions: (actions?: unknown) => actions || { ocr: { on: [] } },
      },
      '@/api/initialization': {},
      '@/utils/imageProcessingConfig': imageProcessing,
      '@/utils/imageAttrDisplay': {},
      '@/utils/modelDefaults': modelDefaults,
      '@/utils/clipboard': {},
      '@/stores/chatResources': { useChatResourcesStore: () => ({ allModels: [], async ensureModels() {} }) },
      '@/stores/editorResources': {
        useEditorResourcesStore: () => ({
          async ensureStorageEngine() {},
          resolveUsableStorageProvider: () => 'local',
        }),
      },
      '@/stores/ui': { useUIStore: () => ({}) },
      '@/stores/auth': { useAuthStore: () => ({ hasRole: () => true }) },
    },
    props,
  )
  if (saved) await page.vm.loadKBData()
  else page.vm.formData = page.vm.initFormData()
  await flush()
  assert.ok(page.vm.formData, 'the editor loads successfully')
  return page.vm
}

test('new document bases submit Wiki indexing and automatic generation by default', async (t) => {
  const vm = await setup(t)
  const data = vm.buildSubmitData()
  assert.equal(data.indexing_strategy.wiki_enabled, true)
  assert.equal(data.wiki_config.auto_generate, true)
  vm.toggleWikiIndexing()
  assert.equal(vm.buildSubmitData().indexing_strategy.wiki_enabled, false, 'the user can opt out')
})

for (const strategy of [undefined, { vector_enabled: true, keyword_enabled: true, wiki_enabled: false }]) {
  test(`editing a ${strategy ? 'saved opt-out' : 'legacy'} base keeps Wiki disabled`, async (t) => {
    const vm = await setup(t, { indexing_strategy: strategy })
    assert.equal(vm.buildSubmitData().indexing_strategy.wiki_enabled, false)
  })
}

test('FAQ creation does not submit document Wiki settings', async (t) => {
  const vm = await setup(t)
  vm.formData.type = 'faq'
  const data = vm.buildSubmitData()
  assert.equal(data.indexing_strategy, undefined)
  assert.equal(data.wiki_config, undefined)
})
