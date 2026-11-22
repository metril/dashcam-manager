import { describe, expect, it } from 'vitest'
import { cleanupSkipReasons, type CleanupStats } from './ingestApi'

const base: CleanupStats = {
  dry_run: true,
  items: [],
  segments_deleted: 0,
  bytes_deleted: 0,
  segments_skipped: 0,
  dirs_removed: 0,
}

describe('cleanupSkipReasons', () => {
  it('returns only non-zero reasons', () => {
    expect(cleanupSkipReasons({ ...base, dest_mismatch: 3, size_mismatch: 0, remove_failed: 1 })).toEqual([
      'destination copy missing or changed: 3',
      'remove failed: 1',
    ])
  })
  it('handles null and all-zero', () => {
    expect(cleanupSkipReasons(null)).toEqual([])
    expect(cleanupSkipReasons(base)).toEqual([])
  })
})
