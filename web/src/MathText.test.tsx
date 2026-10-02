import { act, StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { afterEach, beforeEach, expect, it } from 'vitest'
import MathText from './MathText'
import quantumAbstract from './fixtures/arxiv-2609.40351v1-abstract.txt?raw'

let container: HTMLDivElement
let root: ReturnType<typeof createRoot>

beforeEach(() => {
  Object.assign(globalThis, { IS_REACT_ACT_ENVIRONMENT: true })
  container = document.createElement('div')
  document.body.append(container)
  root = createRoot(container)
})
afterEach(async () => {
  await act(() => root.unmount())
  container.remove()
})
async function render(text: string, inline = false) {
  await act(() => root.render(<StrictMode><MathText text={text} inline={inline} /></StrictMode>))
}

it('同时识别四种定界符，保留文本和换行', async () => {
  await render(String.raw`行内 $x_i^2$ 与 \(\alpha\)。
块级 $$\frac{a}{b}$$ 和 \[\sum_{i=1}^{n}i\]。`)
  expect(container.querySelectorAll('.katex')).toHaveLength(4)
  expect(container.querySelectorAll('.katex-display')).toHaveLength(2)
  expect(container.querySelectorAll('math')).toHaveLength(4)
  expect(container.textContent).toContain('行内 ')
  expect(container.textContent).toContain('\n块级 ')
})

it('解析 arXiv 合并为单行的块级公式', async () => {
  await render(String.raw`Before $$\frac{a}{b}$$ after \[x^2\] end.`)
  expect(container.querySelectorAll('.katex-display')).toHaveLength(2)
})

it('列表和标题将块级公式压为行内公式', async () => {
  await render('$$x^2$$', true)
  expect(container.querySelectorAll('.katex')).toHaveLength(1)
  expect(container.querySelector('.katex-display')).toBeNull()
})

it('切换论文或切回普通文本时移除旧公式，StrictMode 不重复渲染', async () => {
  await render('$x_i$')
  expect(container.querySelectorAll('.katex')).toHaveLength(1)
  await render('$y_j$')
  expect(container.querySelectorAll('.katex')).toHaveLength(1)
  expect(container.querySelector('annotation')?.textContent).toBe('y_j')
  await render('暂无公式。')
  expect(container.querySelector('.katex')).toBeNull()
  expect(container.textContent).toBe('暂无公式。')
})

it('渲染真实摘要中的多项式复杂度宏和 Unicode epsilon', async () => {
  await render(String.raw`two-qubit gates of depth $\poly(n,\log 1/ε)$ with $2^{O(n)}$ ancilla qubits.`)
  expect(container.querySelectorAll('.katex')).toHaveLength(2)
  expect(container.querySelectorAll('math')).toHaveLength(2)
  expect(container.querySelector('.katex-html')?.textContent).toContain('poly')
  expect(container.querySelector('.math-text')?.textContent).not.toContain('$')
})

it('完整真实摘要的六处公式均能渲染，poly 与官方 HTML 的无衬线字体一致', async () => {
  await render(quantumAbstract)
  expect(container.querySelectorAll('.katex')).toHaveLength(6)
  expect(container.querySelectorAll('math')).toHaveLength(6)
  expect(container.querySelectorAll('.katex-html .mathsf')).toHaveLength(1)
  expect(container.querySelector('.math-text')?.textContent).not.toContain('$')
})

it('论文定义的宏不会泄漏到下次渲染', async () => {
  await render(String.raw`$\gdef\poly{BAD}\poly(n)$`)
  expect(container.querySelector('.katex-html')?.textContent).toContain('BAD')
  await render(String.raw`$\poly(n)$`)
  expect(container.querySelector('.katex-html')?.textContent).toContain('poly')
  expect(container.querySelector('.katex-html')?.textContent).not.toContain('BAD')
})

it('非法或未闭合公式保留原文，正常公式继续渲染', async () => {
  const invalid = String.raw`$\unknowncommand{x}$`
  await render(`${invalid} 正常 $x^2$ 未闭合 $z`)
  expect(container.textContent).toContain(invalid)
  expect(container.textContent).toContain('未闭合 $z')
  expect(container.querySelectorAll('.katex')).toHaveLength(1)
})

it('普通文本保持原样且不注入 HTML 或可信 TeX 链接', async () => {
  await render(String.raw`<img src=x onerror=alert(1)> & 普通文字
$\href{javascript:alert(1)}{x}$`)
  expect(container.textContent).toContain('<img src=x onerror=alert(1)> & 普通文字\n')
  expect(container.querySelector('img, a, script')).toBeNull()
})
