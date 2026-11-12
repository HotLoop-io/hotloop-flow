// The dashboard the ui_ nodes draw, in a real browser against the real binary:
// widgets show what messages say, update while the page is open, and what the
// operator does on the page comes back out of the node as a message.

import { expect, test } from '@playwright/test';
import { press, seed } from './helpers';

const tab = 't1';

// HOTLOOP_FLOW_E2E_SHOTS keeps the screenshots, as evidence of what the page looked like.
const shot = (name: string) => (process.env.HOTLOOP_FLOW_E2E_SHOTS ? `${process.env.HOTLOOP_FLOW_E2E_SHOTS}/${name}` : test.info().outputPath(name));

const wire = (...ids: string[]) => [ids];

const flows = [
  { id: tab, type: 'tab', label: '1881 Dashboard' },
  { id: 'ui-base', type: 'ui_base', theme: { name: 'theme-dark', darkTheme: { baseColor: '#097479' }, lightTheme: { baseColor: '#0094CE' } } },
  { id: 'ui-tab', type: 'ui_tab', name: 'Main Screen', icon: 'dashboard', order: 1, disabled: false, hidden: false },
  { id: 'ui-tab2', type: 'ui_tab', name: 'Alarms', icon: 'dashboard', order: 2, disabled: false, hidden: false },
  { id: 'ui-grp', type: 'ui_group', name: 'Heater 1', tab: 'ui-tab', order: 1, disp: true, width: '8', collapse: false },
  { id: 'ui-grp2', type: 'ui_group', name: 'Alarm list', tab: 'ui-tab2', order: 1, disp: true, width: '6', collapse: false },
  { id: 'ui-link', type: 'ui_link', name: 'Dryer 2', link: 'http://192.0.2.7:1880/ui', icon: 'open_in_browser', target: 'newtab', order: 9 },
  // inputs
  { id: 'temp', type: 'inject', z: tab, name: 'temp', props: [{ p: 'payload', v: '151.5', vt: 'num' }, { p: 'topic', v: 'heater', vt: 'str' }], payload: '151.5', payloadType: 'num', topic: 'heater', repeat: '', crontab: '', once: false, x: 100, y: 40, wires: wire('txt', 'num', 'gauge', 'chart') },
  { id: 'hot', type: 'inject', z: tab, name: 'hot', props: [{ p: 'payload', v: 'true', vt: 'bool' }, { p: 'topic', v: 'hot', vt: 'str' }], payload: 'true', payloadType: 'bool', topic: 'hot', repeat: '', crontab: '', once: false, x: 100, y: 80, wires: wire('led', 'tpl') },
  { id: 'cold', type: 'inject', z: tab, name: 'cold', props: [{ p: 'payload', v: 'false', vt: 'bool' }, { p: 'topic', v: 'hot', vt: 'str' }], payload: 'false', payloadType: 'bool', topic: 'hot', repeat: '', crontab: '', once: false, x: 100, y: 120, wires: wire('led', 'tpl') },
  // widgets
  { id: 'txt', type: 'ui_text', z: tab, group: 'ui-grp', order: 1, width: 4, height: 1, name: '', label: 'Temp', format: '{{msg.payload}} F', layout: 'row-spread', x: 300, y: 40, wires: [] },
  { id: 'num', type: 'ui_numeric', z: tab, group: 'ui-grp', order: 2, width: 0, height: 0, name: '', label: 'Setpoint<br>F', tooltip: '', wrap: false, passthru: false, topic: 'topic', topicType: 'msg', format: '{{value}}  F', min: 0, max: '3000', step: 1, x: 300, y: 80, wires: wire('echo') },
  { id: 'gauge', type: 'ui_gauge', z: tab, group: 'ui-grp', order: 3, width: '4', height: '3', name: '', gtype: 'gage', title: 'Heater gauge', label: 'F', format: '{{value}}', min: 0, max: 300, colors: ['#00b500', '#e6e600', '#ca3838'], seg1: '100', seg2: '200', x: 300, y: 120, wires: [] },
  { id: 'chart', type: 'ui_chart', z: tab, group: 'ui-grp', order: 4, width: '8', height: '4', name: '', label: 'Heater trend', chartType: 'line', legend: 'true', xformat: 'HH:mm:ss', interpolate: 'linear', nodata: 'Waiting for the heater', ymin: '', ymax: '', removeOlder: 1, removeOlderPoints: '', removeOlderUnit: '3600', x: 300, y: 160, wires: [[]] },
  { id: 'led', type: 'ui_led', z: tab, group: 'ui-grp', order: 5, width: 0, height: 0, name: '', label: 'Over temp', labelPlacement: 'left', labelAlignment: 'left', colorForValue: [{ color: '#4f4a4a', value: 'false', valueType: 'bool' }, { color: '#eb0000', value: 'true', valueType: 'bool' }], allowColorForValueInMessage: false, shape: 'circle', showGlow: true, x: 300, y: 200, wires: [] },
  { id: 'btn', type: 'ui_button', z: tab, group: 'ui-grp', order: 6, width: 0, height: 0, name: '', label: 'Heater 1', tooltip: '', color: '', bgcolor: 'Gray', icon: '', payload: '1', payloadType: 'num', topic: 'press', topicType: 'str', passthru: false, x: 300, y: 240, wires: wire('count') },
  { id: 'sw', type: 'ui_switch', z: tab, group: 'ui-grp', order: 7, width: 0, height: 0, name: '', label: 'Edit mode', tooltip: '', passthru: false, decouple: 'false', topic: 'switch', topicType: 'str', style: '', onvalue: 'false', onvalueType: 'bool', offvalue: 'true', offvalueType: 'bool', oncolor: 'green', offcolor: 'red', x: 300, y: 280, wires: wire('echo') },
  { id: 'tin', type: 'ui_text_input', z: tab, group: 'ui-grp', order: 8, width: 0, height: 0, name: '', label: 'Note', tooltip: '', passthru: false, mode: 'text', delay: '10', topic: 'note', topicType: 'str', sendOnBlur: false, x: 300, y: 320, wires: wire('echo') },
  { id: 'tpl', type: 'ui_template', z: tab, group: 'ui-grp', name: 'Custom LED', order: 9, width: '4', height: '1', format: '<div id="tpl-btn" ng-click="send({payload: toggleState ? false : true, topic: \'template\'})" ng-init="toggleState=false" ng-class="{\'on\': msg.payload === true}">{{(msg.payload === true || msg.payload === 1) ? \'ON\' : \'OFF\'}}</div>\n<script>(function(scope){ scope.$watch(\'msg.payload\', function(p){ scope.toggleState = (p === true); }); })(scope);</script>', templateScope: 'local', fwdInMessages: false, x: 300, y: 360, wires: wire('echo') },
  { id: 'sp', type: 'ui_spacer', name: 'spacer', group: 'ui-grp', order: 10, width: 1, height: 1 },
  // what comes back from the page
  { id: 'count', type: 'function', z: tab, name: 'count presses', func: 'let n = context.get("n") || 0; n++; context.set("n", n); msg.payload = "pressed " + n + " times"; return msg;', outputs: 1, timeout: 0, noerr: 0, initialize: '', finalize: '', libs: [], x: 520, y: 240, wires: wire('log') },
  { id: 'echo', type: 'function', z: tab, name: 'echo', func: 'msg.payload = msg.topic + "=" + JSON.stringify(msg.payload); return msg;', outputs: 1, timeout: 0, noerr: 0, initialize: '', finalize: '', libs: [], x: 520, y: 300, wires: wire('log') },
  { id: 'log', type: 'ui_text', z: tab, group: 'ui-grp2', order: 1, width: 6, height: 1, name: '', label: 'Last operator action', format: '{{msg.payload}}', layout: 'row-spread', x: 700, y: 260, wires: [] },
];

test.describe('the dashboard', () => {
  // Nothing the page does may raise an error in the browser.
  let errors: string[] = [];
  let removedEverything = false; // the browser logs a 404 for the layout when no dashboard node is running
  test.beforeEach(async ({ page, request }) => {
    errors = [];
    removedEverything = false;
    page.on('pageerror', (e) => errors.push(e.message));
    page.on('console', (m) => { if (m.type() === 'error') errors.push(m.text()); });
    await seed(request, flows);
  });
  test.afterEach(() => {
    expect(errors.filter((e) => !(removedEverything && /404/.test(e)))).toEqual([]);
  });

  test('widgets show what messages say, live, and what the operator does comes back as a message', async ({ page, request }) => {
    await page.goto('/ui');
    await expect(page).toHaveURL(/\/ui\/$/);
    await expect(page.locator('#conn')).toHaveAttribute('data-state', 'open');

    // The menu: two tabs by order and the link, which opens in a new tab.
    const menu = page.locator('#menu a');
    await expect(menu).toHaveText(['Main Screen', 'Alarms', 'Dryer 2']);
    await expect(menu.nth(2)).toHaveAttribute('target', '_blank');
    await expect(page.locator('#title')).toHaveText('Main Screen');
    await expect(page.locator('.group h2').first()).toHaveText('Heater 1');

    // A message updates the page that is already open.
    const active = page.locator('.tab.active');
    await expect(active.locator('.w-text .val')).toHaveText('');
    await press(request, 'temp');
    await expect(page.locator('.tab.active .w-text').filter({ hasText: 'Temp' }).locator('.val')).toHaveText('151.5 F');
    await expect(page.locator('.tab.active .w-numeric input')).toHaveValue('151.5');
    await expect(page.locator('.tab.active .w-gauge canvas')).toBeVisible();
    await expect(page.locator('.tab.active .w-chart .nodata')).toBeHidden();
    // The chart drew something: more than one color in its pixels.
    await expect.poll(async () => page.locator('.tab.active .w-chart canvas').evaluate((cv: HTMLCanvasElement) => {
      const d = cv.getContext('2d')!.getImageData(0, 0, cv.width, cv.height).data;
      let lit = 0;
      for (let i = 3; i < d.length; i += 4) if (d[i] > 0) lit++;
      return lit;
    })).toBeGreaterThan(100);
    await expect(page.locator('.tab.active .w-chart .legend')).toContainText('heater');

    // The LED follows msg.payload and the template follows it too.
    const led = page.locator('.tab.active .w-led .led');
    await press(request, 'hot');
    expect(errors).toEqual([]);
    await expect(led).toHaveCSS('background-color', 'rgb(235, 0, 0)');
    await expect(page.locator('#tpl-btn')).toHaveText('ON');
    await expect(page.locator('#tpl-btn')).toHaveClass(/on/);
    await press(request, 'cold');
    await expect(led).toHaveCSS('background-color', 'rgb(79, 74, 74)');
    await expect(page.locator('#tpl-btn')).toHaveText('OFF');
    await expect(page.locator('#tpl-btn')).not.toHaveClass(/on/);

    // The operator's button press is a message out of the node, and the next node answers on the other tab.
    await page.locator('.tab.active .w-button button').click();
    await page.locator('.tab.active .w-button button').click();
    await page.locator('#menu a', { hasText: 'Alarms' }).click();
    await expect(page.locator('.tab.active .w-text .val')).toHaveText('pressed 2 times');
    await page.locator('#menu a', { hasText: 'Main Screen' }).click();

    // The switch sends its inverted on value, and moves at once.
    const sw = page.locator('.tab.active .w-switch .sw');
    await expect(sw).toHaveAttribute('aria-checked', 'false');
    await sw.click();
    await expect(sw).toHaveAttribute('aria-checked', 'true');
    await page.locator('#menu a', { hasText: 'Alarms' }).click();
    await expect(page.locator('.tab.active .w-text .val')).toHaveText('switch=false');
    await page.locator('#menu a', { hasText: 'Main Screen' }).click();

    // A numeric the operator changes, stopped at its limit.
    const num = page.locator('.tab.active .w-numeric input');
    await num.fill('5000');
    await num.press('Enter');
    await page.locator('#menu a', { hasText: 'Alarms' }).click();
    await expect(page.locator('.tab.active .w-text .val')).toHaveText('heater=3000');
    await page.locator('#menu a', { hasText: 'Main Screen' }).click();

    // A text box sends what was typed, after its delay.
    await page.locator('.tab.active .w-input input').fill('check the vent');
    await page.locator('#menu a', { hasText: 'Alarms' }).click();
    await expect(page.locator('.tab.active .w-text .val')).toHaveText('note="check the vent"');
    await page.locator('#menu a', { hasText: 'Main Screen' }).click();

    // A template's own button sends through scope.send.
    await page.locator('#tpl-btn').click();
    await page.locator('#menu a', { hasText: 'Alarms' }).click();
    await expect(page.locator('.tab.active .w-text .val')).toHaveText('template=true');

    await page.screenshot({ path: shot('dashboard-alarms.png') });
    await page.locator('#menu a', { hasText: 'Main Screen' }).click();
    await page.screenshot({ path: shot('dashboard-main.png'), fullPage: true });
    expect(errors).toEqual([]);
  });

  test('a page that opens late starts from what the widgets last showed', async ({ page, request }) => {
    await press(request, 'temp');
    await page.goto('/ui/');
    await expect(page.locator('.tab.active .w-text').filter({ hasText: 'Temp' }).locator('.val')).toHaveText('151.5 F');
    await expect(page.locator('.tab.active .w-gauge canvas')).toBeVisible();
  });

  test('a redeploy changes the open page', async ({ page, request }) => {
    await page.goto('/ui/');
    await expect(page.locator('.w-button')).toHaveCount(1);
    await seed(request, flows.filter((f) => f.id !== 'btn'));
    await expect(page.locator('.w-button')).toHaveCount(0);
    await expect(page.locator('.w-switch')).toHaveCount(1);
    await seed(request, flows.filter((f) => f.type !== 'ui_tab' && f.type !== 'ui_group' && f.type !== 'ui_link' && f.type !== 'ui_spacer' && !String(f.type).startsWith('ui_')));
    removedEverything = true;
    await expect(page.locator('#empty')).toBeVisible();
  });

  test('the page works at a phone width', async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 800 });
    await page.goto('/ui/');
    await expect(page.locator('.w-button')).toBeVisible();
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
    expect(overflow).toBeLessThanOrEqual(0);
    await page.locator('#menu-btn').click();
    await expect(page.locator('#menu a').first()).toBeVisible();
    await page.screenshot({ path: shot('dashboard-phone.png') });
  });

  test('another site cannot press a button here', async ({ request, baseURL }) => {
    const res = await request.post('/ui/api/emit', {
      headers: { Origin: 'https://evil.example' },
      data: { id: 'btn', event: { event: 'click' } },
    });
    expect(res.status()).toBe(403);
    const same = await request.post('/ui/api/emit', { headers: { Origin: baseURL! }, data: { id: 'btn', event: { event: 'click' } } });
    expect(same.status()).toBe(204);
  });
});
