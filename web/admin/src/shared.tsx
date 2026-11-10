// Shared building blocks for the admin panel pages, split out of App.tsx with
// no behavior changes: the language context and switch, the admin-account
// shape, and the success-rule editing helpers used by both the routes page and
// the alert-rules page.
import { createContext, useContext } from 'react'
import { Select } from 'antd'
import type { Lang, Strings } from './i18n'

export type Me = { username: string; role: string; must_change_password?: boolean }

export type LangCtx = { lang: Lang; setLang: (l: Lang) => void; s: Strings }
export const LangContext = createContext<LangCtx>(null as any)
export const useLang = () => useContext(LangContext)

export function LangSwitch() {
  const { lang, setLang } = useLang()
  return (
    <Select size="small" value={lang} onChange={setLang}
      options={[{ value: 'en', label: 'English' }, { value: 'zh', label: '中文' }]} style={{ width: 96 }} />
  )
}

// buildRule serializes the form's rule editor state into the canonical wire
// shape (also the JSON-tab payload). Returns null when no conditions exist.
export const buildRule = (v: any) => {
  const rows = (v.ruleRows || []).filter((c: any) => c && c.source)
  if (!rows.length) return null
  return {
    mode: v.ruleMode || 'all',
    conditions: rows.map((c: any) => {
      const op = c.op || 'in'
      const vals = (Array.isArray(c.values) ? c.values : [c.values]).map((x: any) => String(x).trim()).filter(Boolean)
      return {
        source: c.source, op,
        path: c.source === 'body' ? (c.path || '') : undefined,
        values: SINGLE_VALUE_OPS.includes(op) ? vals.slice(0, 1) : vals,
      }
    }),
  }
}

// Success-rule operator metadata shared by the route form.
export const SINGLE_VALUE_OPS = ['eq', 'neq', 'gt', 'gte', 'lt', 'lte']

export const NUMERIC_OPS = ['gt', 'gte', 'lt', 'lte']

export const OP_OPTIONS = (s: Strings) => [
  { value: 'in', label: s.routes.ruleIn },
  { value: 'not_in', label: s.routes.ruleNotIn },
  { value: 'eq', label: s.routes.ruleEq },
  { value: 'neq', label: s.routes.ruleNeq },
  { value: 'gt', label: s.routes.ruleGt },
  { value: 'gte', label: s.routes.ruleGte },
  { value: 'lt', label: s.routes.ruleLt },
  { value: 'lte', label: s.routes.ruleLte },
  { value: 'contains', label: s.routes.ruleContains },
  { value: 'not_contains', label: s.routes.ruleNotContains },
]
