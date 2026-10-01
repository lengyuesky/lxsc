// 管理模块按需导入；内容版本由服务端注入链接，加载失败后允许重试。
const LXSCAdminModules = (() => {
  const instances = new Map(), pending = new Map(), factories = new Map(), retries = new Map()
  let generation = 0, navigation = 0
  async function load(name, method) {
    const epoch = sessionState.epoch, currentGeneration = generation, currentNavigation = ++navigation
    if (!sessionState.me?.isAdmin) return
    let instance = instances.get(name)
    if (!instance) {
      let task = pending.get(name)
      if (!task) {
        const link = document.querySelector(`[data-admin-module="${name}"]`)
        if (!link) throw new Error('管理模块不存在')
        task = new Promise((resolve, reject) => {
          const script = document.createElement('script')
          script.type = 'module'
          const source = new URL(link.href)
          const attempt = retries.get(name) || 0
          if (attempt) source.searchParams.set('retry', attempt)
          script.src = source.href
          script.integrity = link.integrity
          script.onload = () => { script.remove(); resolve() }
          script.onerror = () => { retries.set(name, attempt + 1); script.remove(); reject(new Error('管理页面加载失败，请重试')) }
          document.head.appendChild(script)
        }).then(() => {
          const dependencies = { $, adminAPI, esc, platName, fmtDur, formatBytes, formatTime, toast, isAbort, LXSCMusic }
          const create = factories.get(name)
          if (!create) throw new Error('管理模块初始化失败')
          const value = create(dependencies)
          instances.set(name, value)
          return value
        }).finally(() => pending.delete(name))
        pending.set(name, task)
      }
      instance = await task
    }
    if (epoch !== sessionState.epoch || currentGeneration !== generation || !sessionState.me?.isAdmin || currentNavigation !== navigation) return
    return instance[method]()
  }
  function reset() {
    generation++
    navigation++
    for (const instance of instances.values()) instance.reset?.()
  }
  return { load, reset, register: (name, create) => factories.set(name, create), cancelLoad: () => { navigation++ } }
})()
