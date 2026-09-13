// 草稿和幂等操作身份只属于本标签页内存，不写入任何浏览器存储。
export class DraftStore {
  private values = new Map<string, unknown>()
  private operations = new Map<string, { payload: string; id: string }>()
  get<T>(key: string): T | undefined {
    return this.values.get(key) as T | undefined
  }
  set(key: string, value: unknown) {
    this.values.set(key, value)
  }
  delete(key: string) {
    this.values.delete(key)
    this.operations.delete(key)
  }
  get dirty() {
    return this.values.size > 0
  }
  clear() {
    this.values.clear()
    this.operations.clear()
  }
  operation(key: string, payload: unknown) {
    const serialized = JSON.stringify(payload)
    const old = this.operations.get(key)
    if (old?.payload === serialized) return old.id
    const id = crypto.randomUUID()
    this.operations.set(key, { payload: serialized, id })
    return id
  }
}
