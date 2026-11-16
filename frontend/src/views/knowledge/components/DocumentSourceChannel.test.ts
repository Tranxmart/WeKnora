import assert from 'node:assert/strict'
import test from 'node:test'
import { createI18n } from 'vue-i18n'
import { fixture } from '../../../test-utils/vueFixture.ts'
import * as files from '../../../utils/files.ts'
import * as references from '../../../utils/referenceSources.ts'
import * as stall from '../../../utils/knowledgeProcessingStall.ts'
import en from '../../../i18n/locales/en-US'
import zh from '../../../i18n/locales/zh-CN'

for (const locale of ['en-US', 'zh-CN'] as const) {
  test(`Document Service provenance uses its connector label in list and cards in ${locale}`, t => {
    const f = fixture(t)
    const i18n = createI18n({
      legacy: false, locale, messages: { 'en-US': en, 'zh-CN': zh },
    })
    const dependencies = {
      'vue-i18n': { useI18n: () => i18n.global },
      '@/utils/files': files,
      '@/utils/referenceSources': references,
      '@/utils/knowledgeProcessingStall': stall,
    }
    const props = {
      items: [], kbId: 'kb', selectedIds: new Set(), batchMode: false,
      canEdit: false, canDownload: false, canMutateKnowledge: false,
      traceVisibleIds: {}, traceAvailableById: {}, moveMenuMode: 'normal',
      moveTargetKbs: [], moveTargetsLoading: false, moveSelectedTargetName: '',
      moveMode: 'reuse_vectors', moveSubmitting: false,
    }
    const list = f.component(new URL('./DocumentListView.vue', import.meta.url), dependencies, props)
    const cards = f.component(new URL('./DocumentCardView.vue', import.meta.url), dependencies, props)
    const item = { id: 'document', channel: 'document_service', type: 'manual', file_name: 'Notes.md' }
    const source = list.vm.getSourceInfo(item)
    assert.equal(source.label, 'Tranxmart Documents')
    assert.equal(source.icon, 'cloud-download')
    assert.equal(cards.vm.getChannelLabel(item.channel), source.label)
    assert.notEqual(source.label, list.vm.getSourceInfo({ ...item, channel: 'web' }).label)
  })
}
