export interface Summary { text: string; model: string; promptVersion: string }
export interface Paper {
  id: string; version: string; title: string; authors: string[]
  publishedAt: string; updatedAt: string; abstract: string; url: string
  digestDate: string; position: number; status: string; summary: Summary | null
}
export interface Digest { date: string; status: string; paperCount: number; summaryCount: number }
export interface DigestDetail extends Digest { message: string; items: Paper[] }
export interface Page<T> { items: T[]; total: number; page: number; pageSize: number }
