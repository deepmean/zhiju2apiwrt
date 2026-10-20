// 每日签到 + 积分任务，独立于路由器运行（给 GitHub Actions 用，也可以在任意机器上手动跑）。
//
//   ZHIJU_TOKEN=<上游 JWT>  node tools/daily-checkin.mjs
//   ZHIJU_GATEWAY=http://<router>:8080 ZHIJU_PANEL_PASSWORD=root node tools/daily-checkin.mjs
//
// 两种模式：
//   1) 直连上游（默认）：只需要会话 JWT，GitHub 的 runner 也能用；
//   2) 走自家网关：设置 ZHIJU_GATEWAY，脚本改调 POST /admin/tasks/run，由网关去做同一件事
//      （适合自建 runner 能连到路由器的场景）。
//
// 退出码：0 正常（含"今天已签到"）；1 上游报错；2 令牌缺失/已过期。
import { appendFileSync } from 'node:fs'

const BASE = (process.env.ZHIJU_BASE || 'https://agent.zhijuai.com').replace(/\/+$/, '')
const TOKEN = (process.env.ZHIJU_TOKEN || process.env.ZH_TOKEN || '').trim()
const GATEWAY = (process.env.ZHIJU_GATEWAY || '').replace(/\/+$/, '')
const PANEL_PASSWORD = process.env.ZHIJU_PANEL_PASSWORD || process.env.GW_KEY || ''

const lines = []
const say = (s) => { lines.push(s); console.log(s) }
const summary = () => {
  // Actions 里把结果贴到 job summary，省得翻日志
  const path = process.env.GITHUB_STEP_SUMMARY
  if (!path) return
  try { appendFileSync(path, lines.join('\n') + '\n') } catch { /* 非 Actions 环境 */ }
}

// ---- 令牌体检：过期的 JWT 不该让整个 job 静默失败 ------------------------------------
function jwtExpDays(token) {
  const parts = token.split('.')
  if (parts.length !== 3) return null
  try {
    const payload = JSON.parse(Buffer.from(parts[1].replace(/-/g, '+').replace(/_/g, '/'), 'base64').toString('utf8'))
    if (!payload.exp) return null
    return { uid: payload.uid, username: payload.username, days: Math.floor((payload.exp * 1000 - Date.now()) / 86400000) }
  } catch { return null }
}

async function viaGateway() {
  if (!PANEL_PASSWORD) {
    say('✗ 设置了 ZHIJU_GATEWAY，但缺少 ZHIJU_PANEL_PASSWORD')
    return 2
  }
  const res = await fetch(`${GATEWAY}/admin/tasks/run`, {
    method: 'POST',
    headers: { authorization: `Bearer ${PANEL_PASSWORD}`, 'content-type': 'application/json' },
    body: '{}',
  })
  const text = await res.text()
  if (!res.ok) { say(`✗ 网关返回 HTTP ${res.status}: ${text.slice(0, 200)}`); return 1 }
  const j = JSON.parse(text)
  say(`网关模式 ${GATEWAY}`)
  say(`  上次执行: ${j.daily?.last_run ?? '-'}`)
  say(`  结果: ${j.daily?.last_result ?? '-'}`)
  return 0
}

async function upstream() {
  if (!TOKEN) { say('✗ 缺少 ZHIJU_TOKEN（上游会话 JWT）'); return 2 }

  const exp = jwtExpDays(TOKEN)
  if (exp) {
    say(`令牌: ${exp.username ?? '?'} #${exp.uid ?? '?'}，剩余 ${exp.days} 天`)
    if (exp.days <= 0) { say('✗ 令牌已过期，需要重新登录后更新 secret'); return 2 }
    if (exp.days <= 14) say(`⚠ 令牌 ${exp.days} 天后过期，记得续期`)
  } else {
    say('令牌: 不是可解析的 JWT（继续尝试）')
  }

  const H = {
    'content-type': 'application/json',
    accept: 'application/json',
    'accept-language': 'zh-CN',
    'x-locale': 'zh-CN',
    origin: BASE,
    referer: BASE + '/',
    'user-agent': 'zhiju2api-daily-checkin/1.0 (+https://github.com/deepmean/zhiju2apiwrt)',
    'x-token-id': `Bearer ${TOKEN}`,
    'x-common-params': 'channel=molica_web_browser',
    plat: 'web_browser',
    'x-platform': 'web_browser',
    ver: '2.2.11',
  }
  // 重试：上游对数据中心/代理 IP 会回 405（不是 401，也不是 404），几分钟后可能就好了。
  // 这个 job 一天只跑一次，多等一会儿比直接失败划算。
  const RETRY_STATUS = new Set([405, 408, 425, 429, 500, 502, 503, 504])
  const post = async (path, body) => {
    let last = null
    for (let attempt = 1; attempt <= 3; attempt++) {
      try {
        const res = await fetch(BASE + path, { method: 'POST', headers: H, body: JSON.stringify(body ?? {}) })
        const text = await res.text()
        let j = null
        try { j = JSON.parse(text) } catch { /* 非 JSON */ }
        if (res.ok || !RETRY_STATUS.has(res.status) || attempt === 3) {
          return { status: res.status, json: j, text }
        }
        last = { status: res.status, json: j, text }
      } catch (e) {
        last = { status: 0, json: null, text: String(e && e.message ? e.message : e) }
        if (attempt === 3) return last
      }
      const wait = attempt === 1 ? 3000 : 9000
      say(`  · ${path} 返回 ${last.status}，${wait / 1000}s 后重试（${attempt}/3）`)
      await new Promise((r) => setTimeout(r, wait))
    }
    return last
  }

  let code = 0

  // 1) 签到。ret=2016 是"已经签到"，属正常。
  const sign = await post('/v1/aimoss/task/sign')

  // 405 = 这个出口 IP 被上游拦（实测家宽正常、GitHub 托管 runner 全 405）。
  // 这是环境限制，不是配置错误：重试无用，也不该每天给你发一封"失败"邮件。
  // 所以这里给警告并正常退出；真正的错误（令牌失效、上游 5xx）才会失败。
  if (sign.status === 405) {
    say('⚠ 上游按出口 IP 拦了请求（HTTP 405）：本次没有签到。')
    say('  实测家宽正常、GitHub 托管 runner 全 405 —— 上游策略，不是配置问题。')
    say('  日常签到请依赖路由器定时任务（SIGNIN_AT=06:06）；或给仓库挂家宽 self-hosted runner。')
    if (process.env.GITHUB_ACTIONS) say('::warning title=被上游 405 拦::本次未签到，属出口 IP 限制；见 README「在 GitHub 上定时签到」')
    return 0
  }

  if (sign.json?.ret === 1) say('✓ 签到成功')
  else if (sign.json?.ret === 2016) say(`• 今天已签到（${sign.json.msg || '已经签到'}）`)
  else { say(`✗ 签到失败：HTTP ${sign.status} ret=${sign.json?.ret} ${sign.json?.msg ?? sign.text.slice(0, 120)}`); code = 1 }

  // 2) 积分任务：列表里出现什么就做完什么。
  const list = await post('/v1/aimoss/task/list')
  const tasks = list.json?.data?.task_list ?? []
  const finishStatus = list.json?.data?.all_task_finish_status
  if (list.json?.ret !== 1) {
    say(`✗ 任务列表读取失败：HTTP ${list.status} ret=${list.json?.ret} ${list.json?.msg ?? ''}`)
    code = 1
  } else if (!tasks.length) {
    say(`• 任务列表为空（all_task_finish_status=${finishStatus}）`)
  } else {
    say(`任务列表 ${tasks.length} 项：`)
    for (const t of tasks) {
      const id = t.task_id ?? t.id ?? t.task_code ?? t.code
      const name = t.title ?? t.task_name ?? t.name ?? ''
      const status = t.status ?? t.finish_status ?? t.state
      if (status === 1) { say(`  • ${id} ${name}（已完成）`); continue }
      // 入参形状上游没有公开，按几个可能的键一起发（未知字段会被忽略）
      const body = { task_id: id, id, task_code: t.task_code ?? id, code: t.code ?? id }
      const r = await post('/v1/aimoss/task/finish', body)
      const ret = r.json?.ret
      if (ret === 1) say(`  ✓ ${id} ${name} 完成`)
      else if (ret === 2016) say(`  • ${id} ${name} 无需完成（2016）`)
      else { say(`  ✗ ${id} ${name} 失败 ret=${ret} ${r.json?.msg ?? ''}`); code = 1 }
    }
  }

  // 3) 余额（顺带确认令牌真的有效）
  const bal = await post('/v1/integral/balance/query')
  const n = bal.json?.data?.integral_balance
  if (n !== undefined) say(`余额: ${n} 灵力`)
  else { say(`✗ 余额读取失败：HTTP ${bal.status}`); code = code || 1 }

  return code
}

const run = GATEWAY ? viaGateway : upstream
let code = 0
try {
  code = await run()
} catch (e) {
  say(`✗ 异常：${e && e.message ? e.message : e}`)
  code = 1
}
summary()
process.exit(code)
