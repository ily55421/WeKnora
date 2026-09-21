import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

import { INTEGRATION_PREVIEW_ITEMS } from './integrations'
import { integrationSectionKey } from './settingsRoute'

/**
 * Settings.vue keeps two independent lists that must stay in sync by hand:
 * `navItems` declares the entries, `navGroups.pickItems([...])` repeats their
 * keys as raw strings. A key that does not resolve is dropped silently by
 * `.filter(Boolean)`, so the entry vanishes from the sidebar while type-check,
 * build and the i18n audit all stay green — an invisible character inside a
 * key was enough to hide an entire section. Parse both lists and assert the
 * mapping is total and unique.
 */
const source = readFileSync(
  new URL('../views/settings/Settings.vue', import.meta.url),
  'utf8',
)

function block(start: RegExp, end: RegExp) {
  const from = source.search(start)
  assert.ok(from >= 0, `could not locate ${start} in Settings.vue — did the file shape change?`)
  const to = source.slice(from).search(end)
  assert.ok(to >= 0, `could not locate ${end} after ${start}`)
  return source.slice(from, from + to)
}

// Invisible characters are what makes a mistyped key undetectable by eye.
const HIDDEN = [
  '\u200B', '\u200C', '\u200D', '\u2060', '\uFEFF',
  '\u00A0', '\u00AD', '\u202A', '\u202B', '\u202C', '\u202D', '\u202E',
]

function keysIn(blockText: string) {
  const keys: string[] = []
  for (const line of blockText.split('\n')) {
    const integration = line.match(/integrationSectionKey\('([^']+)'\)/)
    if (integration) {
      keys.push(integrationSectionKey(integration[1] as (typeof INTEGRATION_PREVIEW_ITEMS)[number]['key']))
      continue
    }
    for (const quoted of line.match(/'([^']+)'/g) ?? []) {
      keys.push(quoted.slice(1, -1))
    }
  }
  return keys
}

const navBlock = block(/const navItems = computed/, /const navGroups = computed/)
const groupBlock = block(/const navGroups = computed/, /\n\}\)/)

const declared = new Set<string>([
  ...[...navBlock.matchAll(/\{ key: '([^']+)'/g)].map((m) => m[1]!),
  ...INTEGRATION_PREVIEW_ITEMS.map((item) => integrationSectionKey(item.key)),
])

const groupEntries = [...groupBlock.matchAll(/pickItems\(\[([\s\S]*?)\]\)/g)]
assert.ok(groupEntries.length >= 6, `expected at least 6 nav groups, found ${groupEntries.length}`)

const grouped = groupEntries.flatMap((entry) => keysIn(entry[1]!))

test('every nav group key resolves to a declared nav item', () => {
  const unresolved = grouped.filter((key) => !declared.has(key))
  assert.deepEqual(
    unresolved,
    [],
    `pickItems() references keys that no nav item declares; .filter(Boolean) drops them and the entry disappears from the sidebar: ${JSON.stringify(unresolved)}`,
  )
})

test('every declared nav item is placed in a group', () => {
  const orphaned = [...declared].filter((key) => !grouped.includes(key))
  assert.deepEqual(
    orphaned,
    [],
    `nav items declared but never grouped, so they render nowhere: ${JSON.stringify(orphaned)}`,
  )
})

test('no nav item is grouped twice', () => {
  const seen = new Set<string>()
  const duplicated = grouped.filter((key) => (seen.has(key) ? true : (seen.add(key), false)))
  assert.deepEqual(duplicated, [], 'a key listed in two groups renders in both sections')
})

test('nav keys contain no invisible characters', () => {
  for (const key of [...declared, ...grouped]) {
    for (const ch of HIDDEN) {
      assert.ok(
        !key.includes(ch),
        `nav key ${JSON.stringify(key)} embeds U+${ch.codePointAt(0)!.toString(16).toUpperCase()}`,
      )
    }
    assert.match(key, /^[\x20-\x7E]+$/, `nav key ${JSON.stringify(key)} has non-ASCII characters`)
  }
})
