// 元数据清理与磁盘压缩分开确认，避免每次清理都占用业务连接执行 VACUUM。
async function maintainMetadata(compact) {
  const message = compact
    ? '数据库压缩会暂时阻塞业务写入，并可能持续数分钟。请先备份并确认当前处于维护窗口，是否继续？'
    : '确定清理无引用元数据？歌单、收藏和播放历史不会被删除，本次不压缩数据库。'
  if (!confirm(message)) return
  const buttons = ['#cleanupMetadataButton', '#compactMetadataButton'].map($)
  buttons.forEach(button => { button.disabled = true })
  try {
    const result = await adminAPI(compact ? '/metadata/compact' : '/metadata/cleanup', { method: 'POST', body: compact ? { confirm: true } : {} })
    const count = result.cleanup || {}
    toast(compact ? '数据库压缩完成' : `已清理 ${count.tracks || 0} 首歌曲、${count.albums || 0} 个专辑、${count.artists || 0} 个歌手缓存；空闲页可供后续写入复用`)
  } catch (error) {
    if (!isAbort(error)) toast(error.message, true)
  } finally {
    buttons.forEach(button => { button.disabled = false })
  }
}
$('#cleanupMetadataButton').addEventListener('click', () => maintainMetadata(false))
$('#compactMetadataButton').addEventListener('click', () => maintainMetadata(true))
