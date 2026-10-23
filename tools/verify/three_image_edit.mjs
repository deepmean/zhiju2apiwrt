// 3 张参考图的真实改图（复现画布那个失败用例）。用升智-1（免费）。
import { readFileSync } from 'node:fs'

const GW = process.env.GW || 'http://192.168.26.1:8080'
const KEY = process.env.GW_KEY || 'sk-zhiju'

// 三张不同颜色的小图（颜色不同才能验证"三张都传上去了"）
const px = (r, g, b) => Buffer.from(
  // 1x1 PNG，最小可用
  Uint8Array.from([
    0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
    0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53,
    0xde, 0x00, 0x00, 0x00, 0x0c, 0x49, 0x44, 0x41, 0x54, 0x08, 0xd7, 0x63, r, g, b, 0x00, 0x02, 0x00,
    0x01, 0xe2, 0x21, 0xbc, 0x33, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae, 0x42, 0x60,
    0x82,
  ]),
)

const prompt = [
  '参考图片编号：图片1、图片2、图片3。请按这些编号理解提示词中的图片引用。',
  '图片1 图片2 图片3',
  '把这三张参考图的主体合成到同一个明亮场景里，写实风格，保持每个主体的身份特征。',
].join('\n')

const form = new FormData()
form.set('model', 'seedream4-image-edit')
form.set('prompt', prompt)
form.set('size', '1024x1024')
form.append('image[]', new Blob([px(0xff, 0x00, 0x00)], { type: 'image/png' }), 'ref1.png')
form.append('image[]', new Blob([px(0x00, 0xff, 0x00)], { type: 'image/png' }), 'ref2.png')
form.append('image[]', new Blob([px(0x00, 0x00, 0xff)], { type: 'image/png' }), 'ref3.png')

console.log('发送 3 个 image[] 字段...')
const t0 = Date.now()
const res = await fetch(`${GW}/v1/images/edits`, {
  method: 'POST',
  headers: { authorization: `Bearer ${KEY}` },
  body: form,
})
const text = await res.text()
console.log(`HTTP ${res.status}  ${((Date.now() - t0) / 1000).toFixed(1)}s`)
if (!res.ok) { console.log(text.slice(0, 400)); process.exit(1) }
const j = JSON.parse(text)
console.log('url:', j.data?.[0]?.url)
console.log('revised_prompt:', String(j.data?.[0]?.revised_prompt || '').slice(0, 120))
