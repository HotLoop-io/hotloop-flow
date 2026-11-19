// A standby install says so at the top of the editor, so nobody wonders why its
// equipment writes are held; a writer says nothing.

import { expect, test } from '@playwright/test';
import { seed, signIn } from './helpers';

const flows = [
  { id: 't1', type: 'tab', label: 'Line' },
  { id: 'a1', type: 'inject', z: 't1', name: 'tick', props: [{ p: 'payload', v: '1', vt: 'num' }], x: 100, y: 100, wires: [[]] },
];

test('a standby says so, and the writer does not', async ({ page, request }) => {
  await seed(request, flows);
  let role = 'standby';
  await page.route('**/failover', (route) =>
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ role, holder: 'b', name: 'flow', owner: 'plant-a-flow', epoch: 0, current: 4, takeovers: 0, refused: {} }) }));
  await signIn(page, 'a1');
  const banner = page.locator('[data-test="failover-banner"]');
  await expect(banner).toBeVisible();
  await expect(banner).toHaveAttribute('data-role', 'standby');
  await expect(banner).toContainText('standby');
  await expect(banner).toContainText('plant-a-flow');

  // A holder that lost its lease says that instead.
  role = 'lost';
  await page.reload();
  await expect(page.locator('[data-test="failover-banner"]')).toHaveAttribute('data-role', 'lost', { timeout: 15_000 });
  await expect(page.locator('[data-test="failover-banner"]')).toContainText('lost the lease');

  // The writer shows no banner.
  role = 'active';
  await page.reload();
  await expect(page.locator('[data-test="failover-banner"]')).toHaveCount(0);
});
