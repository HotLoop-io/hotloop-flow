// Writes cronosjs-1.7.1.json: what cronosjs 1.7.1, the scheduler inside Node-RED
// 5's Inject node, says comes next for a set of crontabs, start times and zones.
// The Go port has to give the same answer for every one of them.
//
// Node-RED calls cronosjs with no timezone, so it schedules in the process's own
// local time. This does the same: it runs one child process per zone with TZ set,
// which is the path Node-RED takes, rather than passing cronosjs a timezone
// option, which is a different code path inside it.
//
//   npm install cronosjs@1.7.1
//   node golden.mjs > cronosjs-1.7.1.json

import { spawnSync } from 'node:child_process'
import { readFileSync } from 'node:fs'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'

const require = createRequire(import.meta.url)

// How many firings to ask for from each start.
const N = 6

// Zones with daylight saving on both sides of the equator, one with a half-hour
// offset that also shifts, one with a half-hour offset that doesn't, and UTC.
const zones = {
  'Europe/London': ['2026-03-29T01:00:00Z', '2026-10-25T01:00:00Z'],
  'America/New_York': ['2026-03-08T07:00:00Z', '2026-11-01T06:00:00Z'],
  'Australia/Sydney': ['2026-04-04T16:00:00Z', '2026-10-03T16:00:00Z'],
  'America/St_Johns': ['2026-03-08T05:30:00Z', '2026-11-01T04:30:00Z'],
  'Asia/Kolkata': [],
  'UTC': [],
}

// Starts that matter everywhere: the dates cronosjs's own tests use, a leap
// day, a year end, and a time with milliseconds on it.
const generic = [
  '2019-04-21T10:23:45Z',
  '2024-02-28T23:59:59Z',
  '2025-12-31T23:59:30Z',
  '2026-10-03T12:00:00.500Z',
]

const exprs = [
  // From cronosjs's own tests.
  '* * * * *', '17 * * * *', '17 5 * * *', '* 5 * * *', '17 5 31 * *', '17 5 29 Feb *',
  '*/5 * * * *', '17-43/5 * * * *', '17,43 * * * *', '17 5 * * Tue', '17 5 * * 2',
  '17 5 * * 7', '17 5 * * Sun', '17 5 1 * Mon', '* * * * * *', '0 0 L * *',
  '0 0 * * WedL', '0 0 1W * *', '0 0 30W * *', '0 0 LW * *', '0 0 * * Fri#2',
  '0 0 * * Thu#5', '0 10 16 4,L Jun * 2035', '0 10 16 1 1 * */3', '0 10 16 1 1 * 2022/1',
  '0 10 16 1 1 * 2012-2030/3', '5/20 1 * * *', '5/20 2 * * *', '5/20 1,2 * * *',
  '*/20 1 * * *', '0 0 0 1 * ? *', '@hourly', '@midnight', '@daily', '@weekly',
  '@monthly', '@yearly', '@annually',
  // What Node-RED's own Inject dialog writes.
  '00 12 * * *', '30 06 * * 1,2,3,4,5', '*/5 * * * *', '*/10 8-17 * * 1,2,3,4,5',
  '0 6-18 * * 0,6', '15 */2 * * *', '*/1 1-2 * * 3',
  // Shift starts and stops around the hours that go missing or repeat.
  '0 2 * * *', '30 2 * * *', '0 1 * * *', '30 1 * * *', '0 3 * * *', '*/15 1-3 * * *',
  '0 0 * * *', '0 * * * *', '59 1 * * *', '0 0 2 * * *', '30 59 1 * * *',
  // The extended syntax, written every way it can be.
  '*/30 * * * * *', '0 0 12 * * MON-FRI', '0 30 1 * * SAT-MON', '50-10 * * * *',
  '0 0 22-2 * * *', '0 0 0 * NOV-FEB *', '0 0 0 * * fri-mon', '0 0 0 * * 0-6',
  '0 0 0 * * 7', '0 0 0 18/3W * *', '0 0 0 * * *L', '0 0 0 * * *#2',
  '0 0 0 * * thu-mon/2#4', '0 0 0 1,15 * 1', '0 0 9-17/2 * * *', '15-45/10 * * * *',
  '0 0 0 1 */3 *', '0 0 0 29 2 *', '0 0 0 31 * *', '0 59 23 31 12 * 2026',
  '0 0 0 1 1 * 2027-2030', '0 0 12 ? * MON#1', '0 0 0 L,15 * ?', '0 0 0 5-12W * *',
  '0 0 0 * * Mon-WedL',
]

// Crontabs cronosjs refuses, and a few it accepts that look like it shouldn't.
const parseOnly = [
  '* * * *', '* * * * * * * *', '32-8 * * * *', '* * 23-4 * *', '* * * 0-4 *',
  '* 17-27 * * *', '60 * * * *', '* 24 * * *', '* * 0 * *', '* * 32 * *',
  '* * * 13 *', '* * * * 8', '? * * * *', '* ? * * *', '* * ? * *', '* * * ? *',
  '* * * * ?', '* * ?/3 * *', '* * * * ?#3', '0 0 * * Tue#0', '0 0 * * Mon#6',
  '0 0 * * Tue#3.5', '0 0 * * Mon#fourth', '*/0 * * * *', 'a * * * *', '1-2-3 * * * *',
  '1/2/3 * * * *', '* * L * *', '* * W * *', '* * 1W,LW * *', '* * * * L', '* * * * 5L',
  '* * * * 1#1,5L', '@reboot', '@every 5m', '', '   ', '0 0 * * MONDAY',
  '0 0 * JANUARY *', '0 0 0 1 1 * 1969', '0 0 0 1 1 * 275760', '* * * * * 1990',
  '0 0 0 1 1 * 2030-2020',
]

if (process.argv[2]) {
  // A child: one zone, set through TZ by the parent.
  const { CronosExpression } = require('cronosjs')
  const starts = [...generic]
  for (const t of zones[process.argv[2]]) {
    // An hour and a half before a change, five minutes before it, and half an
    // hour after it, which in autumn is inside the hour that repeats.
    starts.push(new Date(Date.parse(t) - 90 * 60000).toISOString())
    starts.push(new Date(Date.parse(t) - 5 * 60000).toISOString())
    starts.push(new Date(Date.parse(t) + 30 * 60000).toISOString())
  }
  const cases = []
  for (const expr of exprs) {
    let e
    try {
      e = CronosExpression.parse(expr)
    } catch (err) {
      throw new Error(`cronosjs refuses ${JSON.stringify(expr)}, which is meant to be valid: ${err.message}`)
    }
    for (const from of starts) {
      const fromMs = Date.parse(from)
      const base = Math.floor(fromMs / 1000)
      cases.push([expr, fromMs, e.nextNDates(new Date(fromMs), N).map(d => d.getTime() / 1000 - base)])
    }
  }
  process.stdout.write(JSON.stringify(cases))
} else {
  const { CronosExpression } = require('cronosjs')
  const version = JSON.parse(readFileSync(require.resolve('cronosjs').replace(/dist-node.*$/, 'package.json'), 'utf8')).version
  if (version !== '1.7.1') throw new Error(`cronosjs ${version} is installed, want 1.7.1`)
  const out = { cronosjs: version, n: N, zones: {}, parse: {} }
  for (const zone of Object.keys(zones)) {
    const r = spawnSync(process.execPath, [fileURLToPath(import.meta.url), zone], {
      env: { ...process.env, TZ: zone },
      encoding: 'utf8',
      maxBuffer: 64 << 20,
    })
    if (r.status !== 0) throw new Error(`${zone}: ${r.stderr}`)
    out.zones[zone] = JSON.parse(r.stdout)
  }
  for (const expr of [...exprs, ...parseOnly]) {
    try {
      CronosExpression.parse(expr)
      out.parse[expr] = true
    } catch {
      out.parse[expr] = false
    }
  }
  process.stdout.write(JSON.stringify(out) + '\n')
}
