const test = require('node:test')
const assert = require('node:assert/strict')
const fs = require('node:fs')
const vm = require('node:vm')

// 仅模拟看板使用的 DOM 接口；日期控件、焦点和事件均执行实际页面脚本。
function fixture(days = 7, to = '2026-01-03') {
  const document = { activeElement: null, addEventListener() {} }
  function element(id = '', tagName = 'INPUT', dataset = {}) {
    const attributes = new Map(), classes = new Set()
    return {
      id, tagName, dataset, value: '', innerHTML: '', textContent: '', scrollLeft: 0,
      classList: { toggle(name, enabled) { enabled ? classes.add(name) : classes.delete(name) }, contains: name => classes.has(name) },
      setAttribute: (name, value) => attributes.set(name, String(value)),
      getAttribute: name => attributes.get(name),
      matches: () => false,
      focus() { document.activeElement = this; this.onfocus?.({ target: this }) },
    }
  }
  const daily = Array.from({ length: days }, (_, i) => ({
    day: new Date(Date.parse(to + 'T00:00:00Z') - (days - i - 1) * 86400000).toISOString().slice(0, 10),
    webMs: (i + 1) * 60000, clientMs: i * 30000, totalMs: (i + 1) * 60000 + i * 30000, plays: i + 1,
  }))
  const stats = {
    daily, from: daily[0].day, to, enabledAt: Date.parse(daily[0].day), countedDays: days,
    totalMs: 1200000, webMs: 900000, clientMs: 300000, averageMs: 200000,
    plays: 12, tracks: 4, activeDays: days, unknownDuration: 0, topTracks: [], topUsers: [],
  }
  const elements = new Map()
  const $ = selector => {
    if (!elements.has(selector)) elements.set(selector, element(selector.replace(/^#/, '')))
    return elements.get(selector)
  }
  $('#listeningScope').value = 'me'
  const calendar = daily.map(day => element('', 'BUTTON', { listeningDay: day.day }))
  const chart = daily.map(day => element('', 'rect', { listeningDay: day.day }))
  document.querySelectorAll = selector => selector === '[data-listening-day]' ? [...chart, ...calendar] : selector === 'button[data-listening-day]' ? calendar : []
  document.querySelector = selector => calendar.find(button => selector === `button[data-listening-day="${button.dataset.listeningDay}"]`) || null
  const scope = {
    document, $, esc: value => String(value), setInterval() {},
    LXSCMusic: { RequestGate: class { cancel() {} } },
  }
  const source = fs.readFileSync(require.resolve('../../internal/assets/web/listening.js'), 'utf8')
  vm.runInNewContext(source + '\nglobalThis.ui = { state: listeningState, render: renderListening, select: selectListeningDay }', scope)
  const ui = scope.ui
  ui.state.data = stats
  ui.render(stats)
  return { ui, $, document, stats, daily, calendar, chart }
}

function assertSelected(f, index) {
  const day = f.daily[index].day
  assert.equal(f.ui.state.selected, day)
  assert.equal(f.$('#listeningDaySlider').value, String(index))
  assert.equal(f.$('#listeningDayPicker').value, day)
  assert.deepEqual(f.calendar.filter(button => button.getAttribute('aria-pressed') === 'true').map(button => button.dataset.listeningDay), [day])
  assert.equal(f.calendar[index].tabIndex, 0)
  assert.equal(f.$('#listeningDayPrevious').disabled, index === 0)
  assert.equal(f.$('#listeningDayNext').disabled, index === f.daily.length - 1)
  assert.equal(f.$('#listeningDayToday').disabled, index === f.daily.length - 1)
}

test('图表和日历移入、单纯聚焦不会改日期，点击后才同步选择', () => {
  const f = fixture(), selected = f.ui.state.selected
  for (const element of [f.chart[1], f.calendar[2]]) {
    element.onpointerenter?.({ pointerType: 'mouse' })
    assert.equal(f.ui.state.selected, selected, '鼠标路过不能覆盖正在查看的日期')
    element.focus()
    assert.equal(f.ui.state.selected, selected, '仅获得焦点不能切换日期')
  }
  f.chart[1].onclick()
  assertSelected(f, 1)
  f.calendar[3].onclick()
  assertSelected(f, 3)
})

test('前后一天与今天按钮受范围约束，跨年详情显示完整日期和星期', () => {
  const f = fixture()
  assertSelected(f, 6)
  f.$('#listeningDayPrevious').onclick()
  assertSelected(f, 5)
  f.$('#listeningDayNext').onclick()
  assertSelected(f, 6)
  f.ui.select(f.daily[0].day)
  assertSelected(f, 0)
  f.$('#listeningDayPrevious').onclick()
  assertSelected(f, 0)
  assert.match(f.$('#listeningDayDetail').innerHTML, /2025年12月28日星期日/)
  f.$('#listeningDayToday').onclick()
  assertSelected(f, 6)
  assert.match(f.$('#listeningDayDetail').innerHTML, /2026年1月3日星期六/)
  assert.match(f.$('#listeningDaySlider').getAttribute('aria-valuetext'), /2026年1月3日星期六/)
})

test('日期框拒绝空值与越界值，修正日期或使用其他控件可清除提示', () => {
  const f = fixture(), picker = f.$('#listeningDayPicker')
  for (const value of ['', '2025-01-01', '2026-01-04', '不是日期']) {
    picker.value = value
    picker.onchange({ target: picker })
    assert.equal(f.ui.state.selected, f.stats.to)
    assert.equal(picker.getAttribute('aria-invalid'), 'true')
    assert.match(f.$('#listeningDateError').textContent, /2025-12-28.*2026-01-03/)
  }
  picker.value = f.daily[2].day
  picker.onchange({ target: picker })
  assertSelected(f, 2)
  assert.equal(picker.getAttribute('aria-invalid'), 'false')
  assert.equal(f.$('#listeningDateError').textContent, '')
  picker.value = ''
  picker.onchange({ target: picker })
  f.$('#listeningDayNext').onclick()
  assertSelected(f, 3)
  assert.equal(f.$('#listeningDateError').textContent, '')
})

test('滑块和日历方向键显式切换，月历与全年日历保持各自方向', () => {
  for (const days of [7, 365]) {
    const f = fixture(days), slider = f.$('#listeningDaySlider')
    slider.value = '2'
    slider.oninput({ target: slider })
    assertSelected(f, 2)
    let prevented = false
    f.calendar[2].onkeydown({ key: 'ArrowLeft', preventDefault() { prevented = true } })
    const previous = days === 7 ? 1 : 0
    assertSelected(f, previous)
    assert.equal(prevented, true)
    assert.equal(f.document.activeElement, f.calendar[previous])
    f.calendar[previous].onkeydown({ key: 'End', preventDefault() {} })
    assertSelected(f, days - 1)
    f.calendar.at(-1).onkeydown({ key: 'Home', preventDefault() {} })
    assertSelected(f, 0)
  }
})

test('重新渲染保留选中日期、控件焦点和日历滚动位置', () => {
  const f = fixture()
  f.ui.select(f.daily[1].day)
  f.$('#listeningDayPrevious').focus()
  f.$('.listening-calendar-scroll').scrollLeft = 180
  f.ui.render(f.stats)
  assertSelected(f, 1)
  assert.equal(f.document.activeElement.id, 'listeningDayPrevious')
  assert.equal(f.$('.listening-calendar-scroll').scrollLeft, 180)
  f.ui.state.selected = '2020-01-01'
  f.ui.render(f.stats)
  assertSelected(f, 6)
})
