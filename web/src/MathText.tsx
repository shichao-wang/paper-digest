import { useLayoutEffect, useRef } from 'react'
import renderMathInElement from 'katex/contrib/auto-render'
import 'katex/dist/katex.min.css'

const delimiters = [
  { left: '$$', right: '$$', display: true },
  { left: '$', right: '$', display: false },
  { left: '\\(', right: '\\)', display: false },
  { left: '\\[', right: '\\]', display: true },
]

export default function MathText({ text, inline = false }: { text: string; inline?: boolean }) {
  const ref = useRef<HTMLSpanElement>(null)
  useLayoutEffect(() => {
    const element = ref.current!
    // 子节点由 KaTeX 管理；每次更新先恢复原文，避免与 React 的 DOM 更新冲突。
    element.textContent = text
    renderMathInElement(element, {
      delimiters: inline ? delimiters.map(delimiter => ({ ...delimiter, display: false })) : delimiters,
      trust: false,
      // 与 arXiv LaTeXML HTML 展开的多项式复杂度宏一致；每次渲染独立创建。
      macros: { '\\poly': '\\mathsf{poly}' },
      throwOnError: true,
      // 无法解析时 auto-render 保留带定界符的原文，其他公式继续渲染。
      errorCallback: () => {},
    })
  }, [text, inline])
  return <span className="math-text" ref={ref} />
}
