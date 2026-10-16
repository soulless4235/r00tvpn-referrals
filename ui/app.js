// The addon's page runs in the panel's sandboxed frame: it has no storage and no network of
// its own, and calls the addon through the panel with postMessage.
'use strict'

let nextId = 1
const pending = new Map()

window.addEventListener('message', (event) => {
  const msg = event.data
  if (!msg || msg.type !== 'rpc-result' || !pending.has(msg.id)) return
  const { resolve, reject } = pending.get(msg.id)
  pending.delete(msg.id)
  if (msg.status >= 200 && msg.status < 300) resolve(msg.body)
  else reject(new Error((msg.body && msg.body.message) || 'Ошибка ' + msg.status))
})

function call(method, path, body) {
  const id = nextId++
  return new Promise((resolve, reject) => {
    pending.set(id, { resolve, reject })
    window.parent.postMessage({ type: 'rpc', id, method, path, body }, '*')
  })
}

const $ = (id) => document.getElementById(id)
const fields = ['service_bot', 'promo_code', 'promo_note', 'reward_days', 'trial_tariff']

function date(unix) {
  return new Date(unix * 1000).toLocaleString('ru-RU', { dateStyle: 'short', timeStyle: 'short' })
}

function show(st) {
  const c = st.config
  for (const f of fields) $(f).value = c[f] ?? ''
  $('bot_token').value = ''
  $('token-hint').textContent = c.token_set
    ? 'Токен сохранён. Оставьте поле пустым, чтобы не менять его.'
    : 'Создайте отдельного бота в @BotFather и вставьте его токен.'
  const status = $('bot-status')
  status.className = st.bot_error ? 'error' : 'muted'
  if (!c.token_set) status.textContent = 'Бот ещё не подключён.'
  else if (st.bot_error) status.textContent = 'Telegram не принял токен: ' + st.bot_error
  else if (st.bot_name) status.textContent = 'Бот работает: @' + st.bot_name
  else status.textContent = 'Подключаемся к Telegram…'

  $('referrers').textContent = st.referrers
  $('invited').textContent = st.invited
  $('paid').textContent = st.paid
  $('days').textContent = st.days
  const list = $('recent')
  list.replaceChildren()
  if (!st.recent || st.recent.length === 0) {
    const li = document.createElement('li')
    li.textContent = 'Пока нет'
    list.append(li)
    return
  }
  for (const r of st.recent) {
    const li = document.createElement('li')
    li.textContent =
      date(r.at) +
      ' — друг ' +
      r.friend +
      ', пригласил ' +
      r.referrer +
      (r.days ? ': +' + r.days + ' дн.' : ': не начислено (' + r.skipped + ')')
    list.append(li)
  }
}

async function load() {
  try {
    show(await call('GET', '/status'))
  } catch (e) {
    $('bot-status').className = 'error'
    $('bot-status').textContent = 'Не удалось загрузить: ' + e.message
  }
}

$('config').addEventListener('submit', async (event) => {
  event.preventDefault()
  const button = event.submitter
  button.disabled = true
  $('saved').textContent = ''
  const body = { bot_token: $('bot_token').value }
  for (const f of fields) body[f] = $(f).value
  body.reward_days = Number(body.reward_days)
  try {
    show(await call('PUT', '/config', body))
    $('saved').className = 'muted'
    $('saved').textContent = 'Сохранено.'
  } catch (e) {
    $('saved').className = 'error'
    $('saved').textContent = 'Не сохранено: ' + e.message
  } finally {
    button.disabled = false
  }
})

load()
